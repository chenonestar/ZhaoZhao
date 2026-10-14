//go:build windows

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var instanceMutex windows.Handle

// acquireInstanceLock 用命名互斥量保证同一个数据目录只运行一个找找。
// 这是启动的第一步：在打开日志、索引库之前完成，第二次启动无论多快都不会漏过。
// 互斥量随进程结束自动释放（包括崩溃和被强制结束）。
func acquireInstanceLock(dataDir string) bool {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	h := sha1.Sum([]byte(strings.ToLower(abs)))
	name, _ := windows.UTF16PtrFromString(`Local\ZhaoZhao-` + hex.EncodeToString(h[:8]))
	m, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if m != 0 {
			windows.CloseHandle(m)
		}
		return false
	}
	if m == 0 {
		return true // 创建失败时不阻止启动（退回到 instance.json 检查）
	}
	instanceMutex = m // 一直持有到进程结束
	return true
}
