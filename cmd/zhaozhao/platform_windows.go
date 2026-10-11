//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

func shellExecute(verb, target string) error {
	v, _ := windows.UTF16PtrFromString(verb)
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, v, t, nil, nil, windows.SW_SHOWNORMAL)
}

// openFile 用默认程序打开文件。
func openFile(path string) error { return shellExecute("open", path) }

// revealFile 打开所在文件夹并选中文件。
func revealFile(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	return cmd.Start()
}

// browserCandidates 是 Chromium 内核浏览器的常见位置，用于以「应用窗口」形式显示界面。
func browserCandidates() []string {
	var c []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		c = append(c,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
	}
	return c
}

// launchUI 以无地址栏的应用窗口打开界面；找不到 Edge/Chrome 时用默认浏览器。
func launchUI(url string) error {
	for _, b := range browserCandidates() {
		if _, err := os.Stat(b); err == nil {
			cmd := exec.Command(b, "--app="+url, "--window-size=1280,800", "--no-first-run")
			if err := cmd.Start(); err == nil {
				return nil
			}
		}
	}
	return shellExecute("open", url)
}

var (
	procFindWindow          = user32.NewProc("FindWindowW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
)

// uiWindow 按标题找到界面窗口（页面标题为「找找」）。
func uiWindow() uintptr {
	t, _ := windows.UTF16PtrFromString("找找")
	h, _, _ := procFindWindow.Call(0, uintptr(unsafePointer(t)))
	return h
}

// showOrLaunchUI 呼出已有窗口，没有就新开一个。
func showOrLaunchUI(url string) error {
	if h := uiWindow(); h != 0 {
		procShowWindow.Call(h, 9) // SW_RESTORE
		procSetForegroundWindow.Call(h)
		return nil
	}
	return launchUI(url)
}

// hideUI 隐藏界面窗口（保持页面状态，下次呼出更快）。
func hideUI() bool {
	h := uiWindow()
	if h == 0 {
		return false
	}
	procShowWindow.Call(h, 0) // SW_HIDE
	return true
}
