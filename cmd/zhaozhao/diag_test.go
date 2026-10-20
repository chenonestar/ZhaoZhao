package main

import (
	"archive/zip"
	"io"
	"log"
	"os"
	"path/filepath"
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
	dir := t.TempDir()
	lg, err := logx.Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
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
	for _, limit := range []time.Duration{30 * time.Second, time.Nanosecond} {
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
		if limit == time.Nanosecond && !strings.Contains(space, "已跳过") {
			t.Fatalf("超时应跳过空间统计：%q", space)
		}
		if limit > time.Second && !strings.Contains(space, "索引库") {
			t.Fatalf("空间统计：%q", space)
		}
		if !strings.Contains(strings.Join(steps, "|"), "索引库空间统计") || !strings.HasPrefix(steps[len(steps)-1], "日志") {
			t.Fatalf("进度：%v", steps)
		}
		os.Remove(p)
		time.Sleep(1100 * time.Millisecond) // 文件名按秒命名
	}
	diagSpaceLimit = 30 * time.Second
}
