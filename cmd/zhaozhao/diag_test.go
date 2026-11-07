package main

import (
	"archive/zip"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

// 导出诊断信息：逐步报告进度；空间统计超时就跳过；写完才出现 zip（中途不会留下打不开的文件）。
func TestExportDiagnostics(t *testing.T) {
	// 日志文件和崩溃输出一直开着（Windows 上删不掉）：不用 t.TempDir，结束时尽量删除
	dir, err := os.MkdirTemp("", "zz-diag-")
	if err != nil {
		t.Fatal(err)
	}
	lg, err := logx.Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		debug.SetCrashOutput(nil, debug.CrashOptions{})
		os.RemoveAll(dir)
	})
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := index.New(db, volume.System(), nil, dir)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	// 第一次模拟超时：用已经过去的时限（1 纳秒不行：Windows 的计时器要过约 1 毫秒才触发，空索引库的统计
	// 几毫秒就做完了，来不及超时），应写简要统计；第二次完整统计；第三次直接用第二次的结果，不再统计
	for i, limit := range []time.Duration{-time.Nanosecond, 30 * time.Second, -time.Nanosecond} {
		diagSpaceLimit = limit
		var steps []string
		p, err := exportDiagnostics(dir, lg, m, true, func(s string) { steps = append(steps, s) })
		if err != nil {
			t.Fatal(err)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(left) > 0 {
			t.Fatalf("留下了临时文件：%v", left)
		}
		zr, err := zip.OpenReader(p)
		if err != nil {
			t.Fatal(err)
		}
		var space string
		for _, f := range zr.File {
			if f.Name == "索引空间.txt" {
				rc, _ := f.Open()
				b, _ := io.ReadAll(rc)
				rc.Close()
				space = string(b)
			}
		}
		zr.Close()
		stepped := strings.Contains(strings.Join(steps, "|"), "索引库空间统计")
		switch i {
		case 0:
			if !strings.Contains(space, "简要统计") || !strings.Contains(space, "索引库") || !stepped {
				t.Fatalf("超时应写简要统计：%q %v", space, steps)
			}
		case 1:
			if !strings.Contains(space, "提取文本") || strings.Contains(space, "统计的结果") || !stepped {
				t.Fatalf("应完整统计：%q", space)
			}
		case 2:
			if !strings.Contains(space, "统计的结果") || !strings.Contains(space, "提取文本") || stepped {
				t.Fatalf("应直接用上次统计的结果：%q %v", space, steps)
			}
		}
		if !strings.HasPrefix(steps[len(steps)-1], "日志") {
			t.Fatalf("进度：%v", steps)
		}
		os.Remove(p)
		time.Sleep(1100 * time.Millisecond) // 文件名按秒命名
	}
	diagSpaceLimit = 2 * time.Minute
}
