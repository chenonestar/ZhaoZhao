package index

import (
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/store"
)

// 阶段用时汇总：各阶段平均用时、写入线程忙的比例、等写入的时间、OCR 页数；什么都没做时不写。
func TestStageStats(t *testing.T) {
	var s stageStats
	if got := s.take(time.Minute, 0); got != "" { // 从第 1 分钟开始统计
		t.Fatalf("什么都没做时不写：%q", got)
	}
	s.addJob(100*time.Millisecond, 200*time.Millisecond, 20*time.Millisecond, false)
	s.addJob(300*time.Millisecond, 0, 0, true)
	start := time.Now()
	p := &prepared{text: &store.Text{Text: "一二三"}, sent: start.Add(-2 * time.Second),
		children: []*preparedChild{{text: &store.Text{Text: "四五"}}}}
	s.addBatch([]*prepared{p}, start, 30*time.Second, true)
	s.addPause(300 * time.Millisecond)
	got := s.take(11*time.Minute, 600) // 这段从第 1 分钟开始
	for _, want := range []string{"最近 10 分钟", "普通文件 2 个（复用已有结果 1 个）", "读取与查重 0.20 秒", "提取 0.10 秒",
		"准备写入 0.01 秒", "写入 1 个文件、5 字", "写入线程忙 5%（其中批量写入 5%）", "平均等写入 2.00 秒",
		"OCR 600 页（每分钟 60 页）", "用电脑时让出 0.30 秒"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺少「%s」：%s", want, got)
		}
	}
	if got := s.take(12*time.Minute, 600); got != "" {
		t.Fatalf("下一段什么都没做：%q", got)
	}
}
