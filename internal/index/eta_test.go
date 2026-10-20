package index

import (
	"testing"
	"time"
)

// 剩余时间按最近一小时实际完成的文件数估算；开始忙不到一小时按实际时长；完成得太少时不估算。
func TestThroughputEta(t *testing.T) {
	at := func(mins ...float64) []time.Duration {
		var out []time.Duration
		for _, m := range mins {
			out = append(out, time.Duration(m*float64(time.Minute)))
		}
		return out
	}
	h := time.Hour
	// 两小时里每 6 分钟完成一个：最近一小时 10 个 → 每个 6 分钟，30 个要 3 小时
	var done []time.Duration
	for m := 3.0; m <= 120; m += 6 {
		done = append(done, at(m)...)
	}
	if got := throughputEta(done, 0, 2*h, 30); got != 3*3600 {
		t.Fatalf("应当按最近一小时的速度估算: %d", got)
	}
	// 一小时前曾经很快（这次开始忙之前的不算）
	if got := throughputEta(append(at(1, 2, 3, 4, 5, 6, 7, 8), at(70, 80, 90, 100, 110, 120)...), 60*time.Minute, 2*h, 6); got != 3600 {
		t.Fatalf("应当只算这次开始忙以来的: %d", got)
	}
	// 刚开始 10 分钟完成 5 个：每个 2 分钟
	if got := throughputEta(at(2, 4, 6, 8, 10), 0, 10*time.Minute, 10); got != 1200 {
		t.Fatalf("开始忙不到一小时时按实际时长: %d", got)
	}
	// 太少：不估算
	if got := throughputEta(at(1, 2, 3), 0, 10*time.Minute, 10); got != -1 {
		t.Fatalf("完成的文件太少时不应估算: %d", got)
	}
	if got := throughputEta(at(0.1, 0.2, 0.3, 0.4, 0.5, 0.6), 0, time.Minute, 10); got != -1 {
		t.Fatalf("开始不到 2 分钟时不应估算: %d", got)
	}
}
