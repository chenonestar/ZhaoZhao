package extract

import "strconv"

// pinMask 返回第 k 个 OCR 组件（每个 t 个线程）固定用的逻辑处理器（位掩码）：依次占用各个物理核心，物理核心
// 用完了再用每个核心的第二个逻辑处理器（超线程）。Windows 把同一个物理核心的两个逻辑处理器编成相邻的号
// （0 和 1 是第一个核心），没有超线程时逻辑处理器就是物理核心。
func pinMask(k, t, cores, logical int) uint64 {
	logical = min(max(1, logical), 64)
	cores = max(1, min(cores, logical))
	smt := logical == 2*cores
	var m uint64
	for i := 0; i < max(1, t); i++ {
		n := k*max(1, t) + i
		cpu := n % logical
		if smt {
			cpu = 2*(n%cores) + (n/cores)%2
		}
		m |= 1 << cpu
	}
	return m
}

// PinSupported 说明能不能把 OCR 组件固定在逻辑处理器上（只有 Windows 能）。
func PinSupported() bool { return pinSupported }

// maskText 把位掩码写成「0、2、4」这样。
func maskText(m uint64) string {
	s := ""
	for i := 0; i < 64; i++ {
		if m&(1<<i) != 0 {
			if s != "" {
				s += "、"
			}
			s += strconv.Itoa(i)
		}
	}
	return s
}
