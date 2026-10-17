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
// 线程预算：只剩 OCR 时用全部物理核心；普通文件、老格式还在处理时用一半。

type ocrPlan struct{ procs, threads int }

func (p ocrPlan) String() string { return fmt.Sprintf("%d 个进程 × %d 线程", p.procs, p.threads) }

const (
	ocrMaxProcs       = 32
	ocrSlotMemDefault = 600 << 20 // 还没实测时，每个 OCR 名额（OCR 进程 + 解析进程）按这么多内存估算
)

// ocrCandidates 返回要实测的搭配：线程预算 budget，最多 maxProcs 个进程。每个进程 1、2、4、8 个线程，
// 预算不超过 8 时再加「1 个进程用全部线程」（以前的默认搭配也在其中，实测不更快就不会换）。
// 进程数受上限限制时，每个进程多分线程，把预算用满。
func ocrCandidates(budget, maxProcs int) []ocrPlan {
	budget, maxProcs = max(1, budget), max(1, min(maxProcs, ocrMaxProcs))
	var out []ocrPlan
	add := func(t int) {
		p := min(max(1, budget/t), maxProcs)
		c := ocrPlan{p, max(1, budget/p)}
		for _, x := range out {
			if x == c {
				return
			}
		}
		out = append(out, c)
	}
	for t := 1; t <= budget && t <= 8; t *= 2 {
		add(t)
	}
	if budget <= 8 {
		add(budget)
	}
	return out
}

// ocrPlanFor 按实测最快的「每个进程几个线程」（best）给出线程预算 budget 下的搭配：
// 进程数 ≈ 预算 ÷ 线程数（四舍五入），不超过 maxProcs，每个进程的线程数把预算分完。
func ocrPlanFor(budget, maxProcs, best int) ocrPlan {
	budget, maxProcs, best = max(1, budget), max(1, maxProcs), max(1, best)
	p := min(max(1, (budget+best/2)/best), maxProcs, budget)
	return ocrPlan{p, max(1, budget/p)}
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

// nextMemCap 由 ocrMemSlots 的结果更新内存允许的进程数 cur（0 表示还没算过）：内存紧张时立即减少；
// 宽裕时要多出半个名额才增加，免得在边界上来回增减进程。
func nextMemCap(cur int, slots float64) int {
	if slots < 0 {
		return ocrMaxProcs
	}
	n := min(max(1, int(slots)), ocrMaxProcs)
	if cur == 0 || n < cur || slots >= float64(cur)+1.5 {
		return n
	}
	return cur
}

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
