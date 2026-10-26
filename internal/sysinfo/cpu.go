package sysinfo

import (
	"fmt"
	"time"
)

// CPUStat 是两次采样之间 CPU 的情况（CPUMonitor.Read）。只有 Windows 取得到，其他系统 OK 为 false。
//
// 某位用户的 i5-12500 识别同一张样张，每个核心每分钟只有 4.2 页，CI 上的 AMD 服务器是 9.2 页。原因可能是
// CPU 降频（电源计划、散热、主板功耗限制），也可能是别的程序在占 CPU。测速和每 10 分钟的统计里记下这些，
// 才能知道慢在哪。
type CPUStat struct {
	OK     bool
	Span   time.Duration
	Busy   float64 // 平均有几个逻辑处理器在忙（全部程序）
	Self   float64 // 其中本程序和它的子进程（解析、OCR 进程）占几个
	PerfOK bool
	Perf   float64 // CPU 实际运行速度是额定频率的百分之几（超过 100 表示睿频；任务管理器的「速度」= 额定 × 这个比例）
	MHz    float64 // 额定频率（MHz）
}

// Others 返回别的程序平均占几个逻辑处理器。
func (s CPUStat) Others() float64 { return max(0, s.Busy-s.Self) }

// ActualGHz 返回实际运行速度（GHz）。
func (s CPUStat) ActualGHz() float64 { return s.MHz * s.Perf / 100 / 1000 }

// String 写成日志里的一句话，如「CPU 实际约 3.9 GHz（额定 3.0 GHz 的 131%），全部程序平均占 11.5 个逻辑处理器，其中别的程序 0.4 个」。
func (s CPUStat) String() string {
	if !s.OK {
		return ""
	}
	out := ""
	if s.PerfOK && s.MHz > 0 {
		out = fmt.Sprintf("CPU 实际约 %.1f GHz（额定 %.1f GHz 的 %.0f%%），", s.ActualGHz(), s.MHz/1000, s.Perf)
	}
	return out + fmt.Sprintf("全部程序平均占 %.1f 个逻辑处理器，其中别的程序 %.1f 个", s.Busy, s.Others())
}

// Slow 判断 CPU 是否明显被限速：大半的逻辑处理器在忙，实际速度却不到额定频率的 80%（正常满负荷时会睿频到额定以上）。
func (s CPUStat) Slow(logical int) bool {
	return s.OK && s.PerfOK && s.MHz > 0 && logical > 0 && s.Busy >= float64(logical)/2 && s.Perf < 80
}
