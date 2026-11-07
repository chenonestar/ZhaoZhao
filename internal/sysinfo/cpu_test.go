package sysinfo

import (
	"strings"
	"testing"
	"time"
)

func TestCPUStat(t *testing.T) {
	s := CPUStat{OK: true, Span: 10 * time.Minute, Busy: 11.5, Self: 11.1, PerfOK: true, Perf: 131, MHz: 3000}
	if got := s.String(); !strings.Contains(got, "CPU 实际约 3.9 GHz（额定 3.0 GHz 的 131%）") || !strings.Contains(got, "别的程序 0.4 个") {
		t.Fatal(got)
	}
	if s.Slow(12) {
		t.Fatal("睿频时不算限速")
	}
	s.Perf = 60
	if !s.Slow(12) {
		t.Fatal("满负荷只有额定 60% 应算限速")
	}
	s.Busy = 3
	if s.Slow(12) {
		t.Fatal("CPU 不忙时降频是正常的")
	}
	if (CPUStat{}).String() != "" || (CPUStat{}).Slow(12) {
		t.Fatal("取不到数据时不写、不提示")
	}
	if c := NewCPUMonitor(nil).Read(); c.OK && c.Busy < 0 {
		t.Fatalf("%+v", c)
	}
}
