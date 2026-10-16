package awake

import (
	"testing"
	"time"
)

func TestTimerSkipsSleep(t *testing.T) {
	t0 := time.Now()
	tm := NewTimer(150 * time.Millisecond)
	defer tm.Stop()
	SimulateSleep(100 * time.Millisecond) // 计时开始后「睡了」100 毫秒：醒着的时间要再多等 100 毫秒
	<-tm.C
	if d := time.Since(t0); d < 240*time.Millisecond {
		t.Fatalf("睡眠的时间不应计入：%v 就到期了", d)
	}
	if s := Slept(); s < 90*time.Millisecond {
		t.Fatalf("Slept = %v", s)
	}
}

func TestTimerStop(t *testing.T) {
	tm := NewTimer(30 * time.Millisecond)
	tm.Stop()
	select {
	case <-tm.C:
		t.Fatal("停止后不应到期")
	case <-time.After(80 * time.Millisecond):
	}
}
