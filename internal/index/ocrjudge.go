package index

import (
	"context"
	"math/rand/v2"
	"slices"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/norm"
)

// 判断一个模型比基准「多认出的内容」是文件里真有的字，还是认错的乱码（0.2.9 某位用户的 OCR 引擎对比里，
// Windows 自带 OCR 有的文件比 PP-OCRv6 多认出七倍的字，PP-OCRv6 认出的它也几乎都有，只看数字分不出真假）。
// 只比较相邻的两个汉字（全文索引按字建，搜词时要相邻的字都对），两个办法：
//   - 另一个引擎也认到了：Windows 多认的，看 PP-OCRv6 大图找字认没认到；大图找字多认的，看 Windows 认没认到。
//     两个完全不同的引擎碰巧认出同样的乱码几乎不可能。
//   - 像不像真字：拿这台电脑上文件里本来就有的文字（文字层、Word、Excel 等，不是 OCR 认出的）当字典，看多认出的
//     相邻两字在字典里出现过的比例。对照两组：两个模型都认出的（真字），把多认出的字打乱重新配对（字一样、
//     组合是乱的）。像真字的程度 = 多认出的比打乱的高出多少 ÷ 真字比打乱的高出多少。
// 结果只有统计数字，不含文字。

const (
	judgeMinPairs  = 30     // 多认出的相邻两字少于这么多的文件不判断
	judgeSample    = 300    // 每组最多查这么多个
	judgeDictDocs  = 1500   // 字典最多读这么多份文字
	judgeDictBytes = 60_000 // 每份最多读这么多字节
	judgeDictMin   = 50     // 字典少于这么多份时不算像不像真字
)

// 判断结果
const (
	verdictReal   = "真内容"
	verdictFake   = "像乱码"
	verdictUnsure = "说不准"
)

type pair = [2]rune

// cjkPairs 返回 s 里相邻两个汉字（或假名、谚文）组成的对，去重。
func cjkPairs(s string) map[pair]struct{} {
	out := map[pair]struct{}{}
	var prev rune
	for _, r := range s {
		if !norm.IsCJK(r) {
			prev = 0
			continue
		}
		if prev != 0 {
			out[pair{prev, r}] = struct{}{}
		}
		prev = r
	}
	return out
}

// judgeCount 是一组相邻两字在字典里出现过（至少 2 份文字里有）的个数。
type judgeCount struct{ n, hit int }

func (c judgeCount) rate() float64 {
	if c.n == 0 {
		return 0
	}
	return float64(c.hit) / float64(c.n)
}

func (c *judgeCount) add(o judgeCount) { c.n += o.n; c.hit += o.hit }

// realness 返回像真字的程度（0～100）；对照组分不开（字典太小、样本太少）时返回 -1。
func realness(extra, real, fake judgeCount) float64 {
	if extra.n < 20 || real.n < 20 || fake.n < 20 || real.rate()-fake.rate() < 0.15 {
		return -1
	}
	v := 100 * (extra.rate() - fake.rate()) / (real.rate() - fake.rate())
	return min(100, max(0, v))
}

// verdict 综合两个办法下结论。other、real 为 -1 表示没法算。
func verdict(other, real float64) string {
	switch {
	case real >= 60 || other >= 50:
		return verdictReal
	case real >= 0 && real < 30 && other < 20:
		return verdictFake
	}
	return verdictUnsure
}

// judgeTask 是一个文件上一个模型要判断的多认内容。
type judgeTask struct {
	file, item           int
	extra, real, fake    []pair
	cExtra, cReal, cFake judgeCount
}

// arbiterOf 返回给 model 多认的内容当「另一个引擎」的模型（没有时为空）。
func arbiterOf(model string, models []string) string {
	other := extract.OCRModelV6Big
	if model == extract.OCRModelV6Big {
		other = extract.OCRModelWin
	}
	if model == other || !slices.Contains(models, other) {
		return ""
	}
	return other
}

// samplePairs 随机取最多 n 个。
func samplePairs(set map[pair]struct{}, n int, rng *rand.Rand) []pair {
	out := make([]pair, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b pair) int { // map 的顺序每次不同，先排好序，结果才能重现
		if a[0] != b[0] {
			return int(a[0] - b[0])
		}
		return int(a[1] - b[1])
	})
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// ocrTestDict 读字典：把这台电脑上随机若干份文件本来就有的文字交给 fn，返回读了几份（测试里换成固定的）。
var ocrTestDict = func(ctx context.Context, m *Manager, fn func(string)) int {
	n := 0
	_ = m.DB.RandomTexts(ctx, judgeDictDocs, judgeDictBytes, func(s string) { n++; fn(s) })
	return n
}

// judgeExtras 判断各模型多认出的内容，结果填进 r（文件的 Items、模型的汇总）。texts[i][model] 是第 i 个文件
// 用 model 认出的文字（失败的没有）。
func (m *Manager) judgeExtras(ctx context.Context, r *OCRTestReport, texts []map[string]string, models []string) {
	rng := rand.New(rand.NewPCG(1, 2))
	var tasks []*judgeTask
	want := map[pair]int{} // 要查的相邻两字 → 出现在几份字典文字里
	for fi := range r.Files {
		t := texts[fi]
		base, ok := t[r.Base]
		if !ok {
			continue
		}
		bp := cjkPairs(base)
		for ii := range r.Files[fi].Items {
			it := &r.Files[fi].Items[ii]
			it.ExtraOther, it.ExtraReal = -1, -1
			text, ok := t[it.Model]
			if it.Model == r.Base || !ok {
				continue
			}
			xp := cjkPairs(text)
			extra, both := map[pair]struct{}{}, map[pair]struct{}{}
			for p := range xp {
				if _, ok := bp[p]; ok {
					both[p] = struct{}{}
				} else {
					extra[p] = struct{}{}
				}
			}
			it.Extra = len(extra)
			if len(extra) < judgeMinPairs {
				continue
			}
			if a := arbiterOf(it.Model, models); a != "" {
				if at, ok := t[a]; ok {
					ap, hit := cjkPairs(at), 0
					for p := range extra {
						if _, ok := ap[p]; ok {
							hit++
						}
					}
					it.ExtraOther = 100 * float64(hit) / float64(len(extra))
				}
			}
			jt := &judgeTask{file: fi, item: ii, extra: samplePairs(extra, judgeSample, rng), real: samplePairs(both, judgeSample, rng)}
			var chars []rune // 打乱重配：用多认出的那些字随机两两配对
			for _, p := range jt.extra {
				chars = append(chars, p[0], p[1])
			}
			for range len(jt.extra) {
				p := pair{chars[rng.IntN(len(chars))], chars[rng.IntN(len(chars))]}
				if _, ok := xp[p]; !ok {
					jt.fake = append(jt.fake, p)
				}
			}
			for _, g := range [][]pair{jt.extra, jt.real, jt.fake} {
				for _, p := range g {
					want[p] = 0
				}
			}
			tasks = append(tasks, jt)
		}
	}
	if len(tasks) == 0 {
		return
	}
	m.otest.mu.Lock()
	m.otest.state.Note = "正在读这台电脑上文件里的文字当字典，判断多认出的是不是真字"
	m.otest.mu.Unlock()
	last := map[pair]int{} // 相邻两字最后出现在第几份里（同一份里出现多次只算一次）
	docNo := 0
	r.DictDocs = ocrTestDict(ctx, m, func(s string) {
		docNo++
		var prev rune
		for _, c := range s {
			if !norm.IsCJK(c) {
				prev = 0
				continue
			}
			if prev != 0 {
				p := pair{prev, c}
				if _, ok := want[p]; ok && last[p] != docNo {
					last[p] = docNo
					want[p]++
				}
			}
			prev = c
		}
	})
	count := func(g []pair) judgeCount {
		c := judgeCount{n: len(g)}
		for _, p := range g {
			if want[p] >= 2 {
				c.hit++
			}
		}
		return c
	}
	type sum struct{ extra, real, fake judgeCount }
	sums := map[string]*sum{}
	for _, jt := range tasks {
		it := &r.Files[jt.file].Items[jt.item]
		jt.cExtra, jt.cReal, jt.cFake = count(jt.extra), count(jt.real), count(jt.fake)
		if r.DictDocs >= judgeDictMin {
			it.ExtraReal = realness(jt.cExtra, jt.cReal, jt.cFake)
		}
		it.Verdict = verdict(it.ExtraOther, it.ExtraReal)
		s := sums[it.Model]
		if s == nil {
			s = &sum{}
			sums[it.Model] = s
		}
		s.extra.add(jt.cExtra)
		s.real.add(jt.cReal)
		s.fake.add(jt.cFake)
	}
	for i := range r.Models {
		row := &r.Models[i]
		row.ExtraOther, row.ExtraReal = -1, -1
		if row.Name == r.Base {
			continue
		}
		var other, w float64
		for _, f := range r.Files {
			for _, it := range f.Items {
				if it.Model != row.Name || it.Extra == 0 {
					continue
				}
				row.Extra += it.Extra
				if it.Extra >= judgeMinPairs && it.ExtraOther >= 0 {
					other += it.ExtraOther * float64(it.Extra)
					w += float64(it.Extra)
				}
				switch it.Verdict {
				case verdictReal:
					row.RealFiles++
				case verdictFake:
					row.FakeFiles++
				case verdictUnsure:
					row.UnsureFiles++
				}
			}
		}
		if w > 0 {
			row.ExtraOther = other / w
		}
		if s := sums[row.Name]; s != nil && r.DictDocs >= judgeDictMin {
			row.ExtraReal = realness(s.extra, s.real, s.fake)
			row.DictExtra, row.DictReal, row.DictFake = 100*s.extra.rate(), 100*s.real.rate(), 100*s.fake.rate()
		}
	}
}
