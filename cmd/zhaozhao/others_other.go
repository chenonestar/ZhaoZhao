//go:build !windows

package main

import (
	"log"
	"time"
)

// 非 Windows 平台没有托盘、对话框，也不找别的找找（只用于开发和测试）。

func runningOthers(dataDir string) []otherInstance { return nil }

func waitProcessExit(pid int, timeout time.Duration) bool { return true }

func processStart() time.Time { return time.Now() }

func askChoice(title, main, content string, buttons []string) int { return -1 }

func showMessage(title, text string) { log.Printf("%s：%s", title, text) }
