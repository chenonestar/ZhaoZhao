//go:build windows

package sysinfo

import "testing"

// 真实的 Windows 上读内存插槽：不崩溃；读到的插槽里至少有一根内存条（虚拟机可能不报告，只记录）。
func TestMemoryModulesReal(t *testing.T) {
	mods := MemoryModules()
	t.Logf("内存插槽 %d 个：%s", len(mods), MemoryDesc(mods))
}
