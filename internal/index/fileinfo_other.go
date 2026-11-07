//go:build !windows

package index

import "os"

// isCloudPlaceholder 判断是否为云盘「仅在线」占位文件（仅 Windows 有意义）。
func isCloudPlaceholder(fi os.FileInfo) bool { return false }
