package index

import (
	"fmt"
	"sort"
	"time"
)

// OCR 的进程数 × 每个进程的线程数。
//
// RapidOCR 每个进程默认 4 个线程。实测 A4 扫描页（4 核）：1 个进程 × 4 线程每分钟 16 页，2×2 是 20 页，
// 4×1 是 26 页；只用 2 个核时 1×2 是 11 页，2×1 是 13 页；线程总数超过核心数反而变慢（8×4 只有 8 页）。
// 几个进程各识别各的页几乎没有损耗，一个进程里的几个线程却要互相等待，所以通常「每核一个进程」最快。
// 但最快的搭配因电脑而异（核心数、大小核、缓存、内存带宽），所以在每台电脑上用随包的样张实测
// （ocrbench.go），结果按电脑保存；进程数还受内存限制（每个 OCR 进程连同它的解析进程占几百 MB）。
// 线程预算：只剩 OCR 时用全部物理核心（实测超线程更快时用全部逻辑处理器）；普通文件、老格式还在处理，
// 或者用户正在用电脑时，用物理核心的一半。

type ocrPlan struct{ procs, threads int }

func (p ocrPlan) String() string { return fmt.Sprintf("%d 个进程 × %d 线程", p.procs, p.threads) }

const (
	ocrMaxProcs       = 32
	ocrSlotMemDefault = 600 << 20 // 还没实测时，每个 OCR 名额（OCR 进程 + 解析进程）按这么多内存估算
)

// ocrCandidates 返回线程预算 budget 下要实测的搭配：最多 maxProcs 个进程。每个进程 1、2、4、8 个线程，
// 预算不超过 8 时再加「1 个进程用全部线程」（以前的默认搭配也在其中，实测不更快就不会换）。
// 进程数受上限限制时，每个进程多分线程，把预算用满。
func ocrCandidates(budget, maxProcs int) []ocrPlan {
	budget, maxProcs = max(1, budget), max(1, min(maxProcs, ocrMaxProcs))
	var out []ocrPlan
	for t := 1; t <= budget && t <= 8; t *= 2 {
		out = appendPlan(out, fillPlan(t, budget, maxProcs))
	}
	if budget <= 8 {
		out = appendPlan(out, fillPlan(budget, budget, maxProcs))
	}
	return out
}

// fillPlan：每个进程 t 个线程、用满预算 budget 的搭配；进程数超过 maxProcs 时每个进程多分线程。
func fillPlan(t, budget, maxProcs int) ocrPlan {
	p := min(max(1, budget/t), maxProcs)
	return ocrPlan{p, max(1, budget/p)}
}

// appendPlan 把 c 加进 out（已有的不重复加）。
func appendPlan(out []ocrPlan, c ocrPlan) []ocrPlan {
	for _, x := range out {
		if x == c {
			return out
		}
	}
	return append(out, c)
}

// ocrBenchCandidates 返回测速要比较的搭配：线程预算为物理核心数 cores 的各种搭配；有超线程（逻辑处理器
// logical 多于物理核心）时，再加两种用上全部逻辑处理器的（每个进程 1 线程、2 线程），看超线程对 OCR 有没有用。
func ocrBenchCandidates(cores, logical, maxProcs int) []ocrPlan {
	out := ocrCandidates(cores, maxProcs)
	maxProcs = max(1, min(maxProcs, ocrMaxProcs))
	if logical > cores {
		for _, t := range []int{1, 2} {
			if c := fillPlan(t, logical, maxProcs); c.procs*c.threads > cores {
				out = appendPlan(out, c)
			}
		}
	}
	return out
}

// effPoint：每个进程 threads 个线程时，平均每个线程每分钟识别 perThread 页（由测速结果算出）。
type effPoint struct {
	threads   int
	perThread float64
}

// effCurve 是按线程数排好序的实测效率，用来估算没测过的搭配的速度：没测过的线程数按相邻两个测过的线性
// 插值；比测过的都少时取最少的那个；比测过的都多时按「多出来的线程不再加快」估计（偏保守）。
type effCurve []effPoint

func (c effCurve) at(t int) float64 {
	switch {
	case len(c) == 0:
		return 0
	case t <= c[0].threads:
		return c[0].perThread
	}
	for i := 1; i < len(c); i++ {
		if t <= c[i].threads {
			a, b := c[i-1], c[i]
			return a.perThread + (b.perThread-a.perThread)*float64(t-a.threads)/float64(b.threads-a.threads)
		}
	}
	last := c[len(c)-1]
	return last.perThread * float64(last.threads) / float64(t)
}

// bestPlan 返回线程预算 budget、最多 maxProcs 个进程时估计最快的搭配：每个进程 t 个线程时开
// min(budget÷t, maxProcs) 个进程，速度 ≈ 总线程数 × at(t)；差不多快时选进程多的（各识别各的，最稳）。
// 没有实测数据时每个进程 1 线程（进程数受限时每个进程多分线程，把预算用满）。
//
// 以前按「最快的那种搭配每个进程几个线程」换算：比如 6 核的电脑实测 3 个进程 × 2 线程最快，普通文件
// 还在处理、预算是 3 时换算成 2 个进程 × 1 线程，只用了 2 个线程。现在比较 3×1、1×3 等，把预算用满。
func (c effCurve) bestPlan(budget, maxProcs int) ocrPlan {
	budget, maxProcs = max(1, budget), max(1, maxProcs)
	if len(c) == 0 {
		p := min(budget, maxProcs)
		return ocrPlan{p, max(1, budget/p)}
	}
	var best ocrPlan
	bestRate := -1.0
	for t := 1; t <= budget; t++ {
		p := min(budget/t, maxProcs)
		if r := float64(p*t) * c.at(t); r > bestRate*1.001 {
			best, bestRate = ocrPlan{p, t}, r
		}
	}
	return best
}

// ocrManualPlan：手动设了进程数时，每个进程的线程数 = 核心数 ÷ 进程数（不超额）。
func ocrManualPlan(procs, cores int) ocrPlan {
	return ocrPlan{procs, max(1, cores/procs)}
}

// ocrMemSlots 返回内存够开几个 OCR 名额（可以是小数）：可用内存加上 OCR 名额现在占着的（live 个），
// 扣掉留给用户的部分（至少 1 GB，或总内存的 15%），除以每个名额的内存 slot。取不到内存信息时返回 -1。
func ocrMemSlots(total, avail, slot uint64, live int) float64 {
	if total == 0 || slot == 0 {
		return -1
	}
	reserve := max(uint64(1)<<30, total*15/100)
	usable := float64(avail) + float64(live)*float64(slot) - float64(reserve)
	return usable / float64(slot)
}

// nextMemCap 由 ocrMemSlots 的结果更新内存允许的进程数 cur（0 表示还没算过）：少了半个名额以上才减少，
// 多出一个半名额才增加，免得在边界上来回增减进程（每个名额已按实测峰值的 1.25 倍算，还留着给用户的内存，
// 少一点没关系）。0.2.7 某次识别期间可用内存在边界附近上下波动，OCR 进程数在 9～12 之间变了十几次。
func nextMemCap(cur int, slots float64) int {
	if slots < 0 {
		return ocrMaxProcs
	}
	n := min(max(1, int(slots)), ocrMaxProcs)
	switch {
	case cur == 0:
		return n
	case slots < float64(cur)-0.5:
		return max(1, min(int(slots+0.5), cur-1))
	case slots >= float64(cur)+1.5:
		return n
	}
	return cur
}

// ocrMemUpDelay：内存允许的进程数减少后，至少这么久才再增加（可用内存刚降下来又回升，多半还会再降）。
const ocrMemUpDelay = 10 * time.Minute

// tailThreads：OCR 队列已经空了、只剩 running 个文件在识别（少于进程数）时，把线程预算平分给它们，
// 大文件剩下的页能识别得快一些。返回每个文件的线程数；不需要调整时返回 0。
func tailThreads(p ocrPlan, budget, running int) int {
	if running <= 0 || running >= p.procs {
		return 0
	}
	if t := budget / running; t > p.threads {
		return t
	}
	return 0
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
