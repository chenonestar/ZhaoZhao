package sysinfo

import (
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pdh                       = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery          = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter  = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData   = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedValue  = pdh.NewProc("PdhGetFormattedCounterValue")
	procGetSystemTimes        = kernel32.NewProc("GetSystemTimes")
	procActiveProcCount       = kernel32.NewProc("GetActiveProcessorCount")
	powrprof                  = windows.NewLazySystemDLL("powrprof.dll")
	procPowerGetActiveScheme  = powrprof.NewProc("PowerGetActiveScheme")
	procPowerReadFriendlyName = powrprof.NewProc("PowerReadFriendlyName")
	procCallNtPowerInfo       = powrprof.NewProc("CallNtPowerInformation")
)

const pdhFmtDouble = 0x00000200

// CPUMonitor 每次 Read 返回和上一次之间的 CPU 情况。childrenCPU 返回子进程累计用掉的 CPU 时间。
type CPUMonitor struct {
	mu          sync.Mutex
	childrenCPU func() (time.Duration, bool)
	query       uintptr
	perf, freq  uintptr
	at          time.Time
	busy, self  time.Duration
}

// NewCPUMonitor 开始监测（第一次 Read 返回从这时起的情况）。
func NewCPUMonitor(childrenCPU func() (time.Duration, bool)) *CPUMonitor {
	c := &CPUMonitor{childrenCPU: childrenCPU}
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&c.query))); r == 0 {
		add := func(path string) uintptr {
			var h uintptr
			p, _ := windows.UTF16PtrFromString(path)
			if r, _, _ := procPdhAddEnglishCounter.Call(c.query, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&h))); r != 0 {
				return 0
			}
			return h
		}
		c.perf = add(`\Processor Information(_Total)\% Processor Performance`)
		c.freq = add(`\Processor Information(_Total)\Processor Frequency`)
		procPdhCollectQueryData.Call(c.query)
	}
	c.at, c.busy, c.self = time.Now(), systemBusy(), c.selfCPU()
	return c
}

// systemBusy 返回开机以来全部逻辑处理器忙碌的时间之和。
func systemBusy() time.Duration {
	var idle, kernel, user windows.Filetime
	if r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return 0
	}
	ft := func(f windows.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration(ft(kernel)+ft(user)-ft(idle)) * 100 // 内核时间里包含空闲时间
}

// selfCPU 返回本进程和子进程累计用掉的 CPU 时间。
func (c *CPUMonitor) selfCPU() time.Duration {
	var r windows.Rusage
	var d time.Duration
	if windows.GetProcessTimes(windows.CurrentProcess(), &r.CreationTime, &r.ExitTime, &r.KernelTime, &r.UserTime) == nil {
		d = time.Duration(r.KernelTime.Nanoseconds() + r.UserTime.Nanoseconds())
	}
	if c.childrenCPU != nil {
		if ch, ok := c.childrenCPU(); ok {
			d += ch
		}
	}
	return d
}

func (c *CPUMonitor) value(h uintptr) (float64, bool) {
	if h == 0 {
		return 0, false
	}
	var v struct {
		status uint32
		_      uint32
		value  float64
	}
	if r, _, _ := procPdhGetFormattedValue.Call(h, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&v))); r != 0 || v.status > 1 {
		return 0, false
	}
	return v.value, true
}

// Read 返回和上一次 Read（或创建时）之间的 CPU 情况。
func (c *CPUMonitor) Read() CPUStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, busy, self := time.Now(), systemBusy(), c.selfCPU()
	st := CPUStat{Span: now.Sub(c.at)}
	if st.Span > 0 && busy > 0 {
		st.OK = true
		st.Busy = float64(busy-c.busy) / float64(st.Span)
		st.Self = min(st.Busy, float64(self-c.self)/float64(st.Span))
	}
	if c.query != 0 {
		procPdhCollectQueryData.Call(c.query)
		st.Perf, st.PerfOK = c.value(c.perf)
		st.MHz, _ = c.value(c.freq)
	}
	c.at, c.busy, c.self = now, busy, self
	return st
}

// PowerPlan 返回当前的电源计划名称（如「平衡」「高性能」「节能」）。
func PowerPlan() string {
	var guid *windows.GUID
	if r, _, _ := procPowerGetActiveScheme.Call(0, uintptr(unsafe.Pointer(&guid))); r != 0 || guid == nil {
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(guid)))
	var size uint32
	procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(guid)), 0, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 || size > 4096 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if r, _, _ := procPowerReadFriendlyName.Call(0, uintptr(unsafe.Pointer(guid)), 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// processorPowerInfo 是 PROCESSOR_POWER_INFORMATION。
type processorPowerInfo struct {
	Number, MaxMhz, CurrentMhz, MhzLimit, MaxIdleState, CurrentIdleState uint32
}

// CPULimit 返回处理器的最高频率和当前的频率上限（MHz，取各逻辑处理器中最小的上限）。上限低于最高频率
// 说明被限速了（电源计划的「最大处理器状态」、过热、主板功耗限制）。取不到时返回 0, 0。
func CPULimit() (maxMHz, limitMHz uint32) {
	n := numLogical()
	if n <= 0 {
		return 0, 0
	}
	info := make([]processorPowerInfo, n)
	size := uint32(n) * uint32(unsafe.Sizeof(info[0]))
	const processorInformation = 11
	if r, _, _ := procCallNtPowerInfo.Call(processorInformation, 0, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(size)); r != 0 {
		return 0, 0
	}
	for i, x := range info {
		if i == 0 || x.MaxMhz > maxMHz {
			maxMHz = x.MaxMhz
		}
		if i == 0 || x.MhzLimit < limitMHz {
			limitMHz = x.MhzLimit
		}
	}
	return maxMHz, limitMHz
}

// numLogical 返回逻辑处理器数（全部处理器组）。
func numLogical() int {
	const allGroups = 0xffff
	n, _, _ := procActiveProcCount.Call(allGroups)
	return int(n)
}
