package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// runOCRBench 用随包的 OCR 组件和样张实测本机各种「进程数 × 线程数」的速度和内存，把结果写到 out
// （发布前在 Windows 上跑一遍，确认测速在真实组件上可用，并记录每个 OCR 进程的内存）。
func runOCRBench(out string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	suppressErrorDialogs() // OCR 组件崩溃时不弹出系统错误对话框（会卡住自动测试）
	var b strings.Builder
	logf := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		b.WriteString(line + "\n")
		log.Print(line)
		_ = os.WriteFile(out, []byte(b.String()), 0o644)
	}
	cfg := extract.FindOCR()
	logf("找找 %s；%s", version, sysInfo())
	logf("OCR 组件：%s %s", cfg.Command, strings.Join(cfg.Args, " "))
	pool := worker.NewPool(exe, 1)
	pool.Name = "OCR"
	defer pool.Close()
	t0 := time.Now()
	res, err := index.BenchOCR(context.Background(), pool, cfg, func(s string) { logf("正在测：%s", s) })
	if err != nil {
		return err
	}
	logf("用时 %.0f 秒：%s", time.Since(t0).Seconds(), res.Summary())
	j, _ := json.MarshalIndent(res, "", "  ")
	logf("%s", j)
	if res.Best < 0 {
		return fmt.Errorf("全部搭配都失败了：%s", res.Err)
	}
	return nil
}
