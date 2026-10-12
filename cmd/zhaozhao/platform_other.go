//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/chenonestar/zhaozhao/internal/index"
)

func openFile(path string) error { return exec.Command("xdg-open", path).Start() }

func revealFile(path string) error { return exec.Command("xdg-open", filepath.Dir(path)).Start() }

func launchUI(url string) error { return exec.Command("xdg-open", url).Start() }

func showOrLaunchUI(url string) error { return launchUI(url) }

func hideUI() bool { return false }

func closeUI() {}

func suppressErrorDialogs() {}

type shellCtx struct {
	URL    string
	Mgr    *index.Manager
	Hotkey string
	Quit   func()
}

var stop = make(chan struct{}, 1)

func shutdownShell() {
	select {
	case stop <- struct{}{}:
	default:
	}
}

// runShell 在非 Windows 平台只是等待退出信号（托盘和全局快捷键仅 Windows 实现）。
func runShell(c *shellCtx) {
	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGHUP)
	<-stop
}
