//go:build windows

package index

import (
	"os"
	"syscall"
)

const (
	attrOffline         = 0x1000
	attrRecallOnOpen    = 0x40000
	attrRecallOnDataAcc = 0x400000
)

// isCloudPlaceholder 识别 OneDrive 等云盘中尚未下载的占位文件，避免读取时触发下载（F-IDX-08）。
func isCloudPlaceholder(fi os.FileInfo) bool {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return d.FileAttributes&(attrOffline|attrRecallOnOpen|attrRecallOnDataAcc) != 0
}
