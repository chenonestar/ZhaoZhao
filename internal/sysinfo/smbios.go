package sysinfo

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// 内存条：从 SMBIOS（主板固件报告的硬件表）的「内存设备」（类型 17）读出每个插槽插了什么。某位用户的电脑
// 多个 OCR 进程同时识别时每个都慢了好几倍，很可能是内存带宽不够（只插了一根内存条，单通道），不用打开机箱、
// 不用装软件，诊断信息里就能看出插了几根、多大、跑在什么频率。

// MemModule 是一个内存插槽。
type MemModule struct {
	Slot       string // 插槽名，如「DIMM1」「ChannelA-DIMM0」
	Bank       string // 主板标的位置，如「BANK 0」
	SizeMB     int    // 0 表示没插
	Type       string // 如「DDR4」，不认识的为空
	Speed      int    // 内存条标称的速度（MT/s），0 表示不知道
	Configured int    // 实际运行的速度（MT/s），0 表示不知道
}

// memTypes 是 SMBIOS 内存类型编号对应的名字（只列常见的）。
var memTypes = map[byte]string{0x12: "DDR", 0x13: "DDR2", 0x18: "DDR3", 0x1A: "DDR4", 0x1B: "LPDDR", 0x1C: "LPDDR2",
	0x1D: "LPDDR3", 0x1E: "LPDDR4", 0x22: "DDR5", 0x23: "LPDDR5"}

// parseSMBIOSMemory 从 SMBIOS 表（不含 Windows 加的 8 字节头）里取出全部内存插槽。
func parseSMBIOSMemory(t []byte) []MemModule {
	var out []MemModule
	for len(t) >= 4 {
		typ, n := t[0], int(t[1])
		if n < 4 || n > len(t) {
			break
		}
		// 格式化区之后是字符串区：一串以 0 结尾的字符串，最后再多一个 0
		end := n
		for end+1 < len(t) && !(t[end] == 0 && t[end+1] == 0) {
			end++
		}
		var strs []string
		for _, s := range strings.Split(string(t[n:min(end, len(t))]), "\x00") {
			strs = append(strs, strings.TrimSpace(s))
		}
		str := func(off int) string {
			if off >= n {
				return ""
			}
			i := int(t[off])
			if i < 1 || i > len(strs) {
				return ""
			}
			return strs[i-1]
		}
		word := func(off int) int {
			if off+2 > n {
				return 0
			}
			return int(binary.LittleEndian.Uint16(t[off:]))
		}
		if typ == 17 && n >= 0x15 {
			m := MemModule{Slot: str(0x10), Bank: str(0x11), Speed: word(0x15), Configured: word(0x20)}
			switch sz := word(0x0C); {
			case sz == 0 || sz == 0xFFFF:
			case sz == 0x7FFF && n >= 0x20:
				m.SizeMB = int(binary.LittleEndian.Uint32(t[0x1C:]) & 0x7FFFFFFF)
			case sz&0x8000 != 0:
				m.SizeMB = (sz & 0x7FFF) / 1024 // 以 KB 为单位
			default:
				m.SizeMB = sz
			}
			if n > 0x12 {
				m.Type = memTypes[t[0x12]]
			}
			out = append(out, m)
		}
		if typ == 127 { // 表结束
			break
		}
		t = t[min(end+2, len(t)):]
	}
	return out
}

// MemoryDesc 把内存插槽写成一句话，如「内存条 1 根（共 2 个插槽）：8 GB DDR4 3200 MT/s（实际 2666 MT/s）」。
// 取不到时返回空字符串。
func MemoryDesc(mods []MemModule) string {
	var parts []string
	used := 0
	for _, m := range mods {
		if m.SizeMB <= 0 {
			continue
		}
		used++
		s := fmt.Sprintf("%g GB", float64(m.SizeMB)/1024)
		if m.Type != "" {
			s += " " + m.Type
		}
		if m.Speed > 0 {
			s += fmt.Sprintf(" %d MT/s", m.Speed)
		}
		if m.Configured > 0 && m.Configured != m.Speed {
			s += fmt.Sprintf("（实际 %d MT/s）", m.Configured)
		}
		if loc := strings.TrimSpace(m.Bank + " " + m.Slot); loc != "" {
			s += "，插在 " + loc
		}
		parts = append(parts, s)
	}
	if used == 0 {
		return ""
	}
	return fmt.Sprintf("内存条 %d 根（共 %d 个插槽）：%s", used, len(mods), strings.Join(parts, "；"))
}
