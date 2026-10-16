package index

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// OCR 的进程数 × 每个进程的线程数（S）。
//
// RapidOCR 每个进程默认 4 个线程。线程总数超过 CPU 核心数后互相争抢，总速度不升反降：
// 4 核机器上实测 1 个进程 × 4 线程每分钟 17 页，8 个进程 × 4 线程只有 8 页。
// 最快的搭配因电脑而异（核心数、缓存、内存带宽），所以「自动」时在用户的电脑上实测：
// 在线程预算内比较几种搭配的每分钟页数，选最快的，之后每 2 小时复查一次。
// 线程预算：只剩 OCR 时用全部物理核心；普通文件、老格式还在处理时用一半。

type ocrPlan struct{ procs, threads int }

func (p ocrPlan) String() string { return fmt.Sprintf("%d 个进程 × %d 线程", p.procs, p.threads) }

const (
	maxOCRProcs     = 4               // 每个 OCR 进程占几百 MB 内存
	ocrMeasureMin   = 5 * time.Minute // 每种搭配至少测这么久
	ocrMeasureMax   = 15 * time.Minute
	ocrMeasurePages = 10          // 且至少识别这么多页（页数太少时延长，最长 ocrMeasureMax）
	ocrWarmup       = time.Minute // 换搭配后 OCR 进程要重新加载模型：这段时间不计
	ocrRecheck      = 2 * time.Hour
)

// ocrCandidates 返回线程预算内可选的搭配：进程数 1、2、4，每个进程至少 2 个线程。
func ocrCandidates(budget int) []ocrPlan {
	if budget < 2 {
		return []ocrPlan{{1, 1}}
	}
	var out []ocrPlan
	for p := 1; p <= maxOCRProcs && budget/p >= 2; p *= 2 {
		out = append(out, ocrPlan{p, budget / p})
	}
	return out
}

// ocrDefaultPlan 是没有实测数据时的搭配：每个进程约 4 个线程（RapidOCR 的默认值）。
func ocrDefaultPlan(budget int) ocrPlan {
	c := ocrCandidates(budget)
	best := c[0]
	for _, p := range c {
		if abs(p.threads-4) < abs(best.threads-4) {
			best = p
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ocrManualPlan：手动设了进程数时，每个进程的线程数 = 核心数 ÷ 进程数（不超额）。
func ocrManualPlan(procs, cores int) ocrPlan {
	return ocrPlan{procs, max(1, cores/procs)}
}

// budgetState 是某个线程预算下的实测结果。
type budgetState struct {
	scores  map[ocrPlan]float64 // 每分钟页数
	best    ocrPlan
	settled time.Duration // 选定的时间（awake）；0 表示还在比较
}

// ocrTuner 逐步比较各种搭配。step 定时调用，时间用 awake.Now()（不含电脑睡眠）。
type ocrTuner struct {
	budget     int
	byBudget   map[int]*budgetState
	cur        ocrPlan
	start      time.Duration // 当前测量窗口的开始
	startPages int64
	warmUntil  time.Duration
}

func newOCRTuner() *ocrTuner { return &ocrTuner{byBudget: map[int]*budgetState{}} }

// step 根据线程预算、OCR 通道是否满负荷（所有进程都在忙且还有排队）、累计识别页数，决定当前搭配。
// 返回当前搭配、是否改变，以及要写进日志的说明（没有则为空）。
func (t *ocrTuner) step(budget int, saturated bool, pages int64, now time.Duration) (ocrPlan, bool, string) {
	if budget != t.budget || t.cur.procs == 0 {
		t.budget = budget
		bs := t.byBudget[budget]
		if bs == nil {
			bs = &budgetState{scores: map[ocrPlan]float64{}}
			t.byBudget[budget] = bs
		}
		next := ocrDefaultPlan(budget)
		if bs.settled > 0 {
			next = bs.best
		}
		return t.switchTo(next, pages, now), true, ""
	}
	bs := t.byBudget[budget]
	if now < t.warmUntil || !saturated {
		// 刚换搭配，或者 OCR 不是满负荷（排队的文件不够）：测不准，重新开始计
		t.start, t.startPages = now, pages
		return t.cur, false, ""
	}
	if bs.settled > 0 {
		if now-bs.settled < ocrRecheck {
			return t.cur, false, ""
		}
		bs.scores, bs.settled = map[ocrPlan]float64{}, 0 // 定期复查：从当前搭配重新比较
		t.start, t.startPages = now, pages
		return t.cur, false, ""
	}
	elapsed := now - t.start
	done := pages - t.startPages
	if elapsed < ocrMeasureMin || (done < ocrMeasurePages && elapsed < ocrMeasureMax) {
		return t.cur, false, ""
	}
	bs.scores[t.cur] = float64(done) / elapsed.Minutes()
	for _, c := range ocrCandidates(budget) {
		if _, ok := bs.scores[c]; !ok {
			return t.switchTo(c, pages, now), true, ""
		}
	}
	// 全部测过：选最快的
	var parts []string
	best := t.cur
	for _, c := range ocrCandidates(budget) {
		parts = append(parts, fmt.Sprintf("%s 每分钟 %.1f 页", c, bs.scores[c]))
		if bs.scores[c] > bs.scores[best] {
			best = c
		}
	}
	bs.best, bs.settled = best, now
	note := fmt.Sprintf("OCR 搭配实测（%d 个线程）：%s；选用 %s", budget, strings.Join(parts, "，"), best)
	if best == t.cur {
		return t.cur, false, note
	}
	return t.switchTo(best, pages, now), true, note
}

func (t *ocrTuner) switchTo(p ocrPlan, pages int64, now time.Duration) ocrPlan {
	t.cur = p
	t.warmUntil = now + ocrWarmup
	t.start, t.startPages = now, pages
	return p
}

// describe 返回给界面看的说明：正在比较时写明进度。
func (t *ocrTuner) describe() string {
	bs := t.byBudget[t.budget]
	if bs == nil || bs.settled > 0 {
		return ""
	}
	c := ocrCandidates(t.budget)
	if len(c) < 2 {
		return ""
	}
	return fmt.Sprintf("正在实测最快的搭配（第 %d/%d 种：%s）", len(bs.scores)+1, len(c), t.cur)
}

// rateWindow 记录最近的累计页数，算最近每分钟识别多少页（界面显示用）。
type rateWindow struct {
	at    []time.Duration
	pages []int64
}

func (r *rateWindow) add(now time.Duration, pages int64) {
	r.at, r.pages = append(r.at, now), append(r.pages, pages)
	// 只留最近 10 分钟
	i := sort.Search(len(r.at), func(i int) bool { return now-r.at[i] <= 10*time.Minute })
	r.at, r.pages = r.at[i:], r.pages[i:]
}

func (r *rateWindow) perMin() float64 {
	if len(r.at) < 2 {
		return 0
	}
	d := r.at[len(r.at)-1] - r.at[0]
	if d < time.Minute {
		return 0
	}
	return float64(r.pages[len(r.pages)-1]-r.pages[0]) / d.Minutes()
}
