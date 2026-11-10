package sysinfo

import (
	"encoding/binary"
	"strings"
	"testing"
)

// memDevice 生成一个 SMBIOS 类型 17 结构（长度 0x28，SMBIOS 2.7 起的格式）。
func memDevice(size uint16, ext uint32, typ byte, speed, conf uint16, strs ...string) []byte {
	b := make([]byte, 0x28)
	b[0], b[1] = 17, 0x28
	binary.LittleEndian.PutUint16(b[0x0C:], size)
	b[0x10], b[0x11] = 1, 2
	b[0x12] = typ
	binary.LittleEndian.PutUint16(b[0x15:], speed)
	binary.LittleEndian.PutUint32(b[0x1C:], ext)
	binary.LittleEndian.PutUint16(b[0x20:], conf)
	for _, s := range strs {
		b = append(b, s...)
		b = append(b, 0)
	}
	return append(b, 0)
}

func TestParseSMBIOSMemory(t *testing.T) {
	other := []byte{4, 6, 0, 0, 1, 0, 'C', 'P', 'U', 0, 0} // 别的类型（处理器），带一个字符串
	var tbl []byte
	tbl = append(tbl, other...)
	tbl = append(tbl, memDevice(8192, 0, 0x1A, 3200, 2666, "DIMM1", "BANK 0")...)
	tbl = append(tbl, memDevice(0, 0, 0x02, 0, 0, "DIMM2", "BANK 1")...) // 空插槽
	tbl = append(tbl, memDevice(0x7FFF, 65536, 0x22, 4800, 4800, "DIMM3", "BANK 2")...)
	tbl = append(tbl, 127, 4, 0, 0, 0, 0)
	mods := parseSMBIOSMemory(tbl)
	if len(mods) != 3 || mods[0].SizeMB != 8192 || mods[0].Type != "DDR4" || mods[0].Slot != "DIMM1" || mods[0].Bank != "BANK 0" ||
		mods[0].Configured != 2666 || mods[1].SizeMB != 0 || mods[2].SizeMB != 65536 || mods[2].Type != "DDR5" {
		t.Fatalf("%+v", mods)
	}
	d := MemoryDesc(mods)
	for _, want := range []string{"内存条 2 根（共 3 个插槽）", "8 GB DDR4 3200 MT/s（实际 2666 MT/s），插在 BANK 0 DIMM1", "64 GB DDR5 4800 MT/s，插在"} {
		if !strings.Contains(d, want) {
			t.Fatalf("缺少「%s」：%s", want, d)
		}
	}
	if MemoryDesc(nil) != "" || len(parseSMBIOSMemory([]byte{17, 2})) != 0 {
		t.Fatal("取不到时为空")
	}
}
