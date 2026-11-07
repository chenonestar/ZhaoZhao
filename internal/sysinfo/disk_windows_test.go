package sysinfo

import (
	"os"
	"strings"
	"testing"
)

// 取得到就是「固态硬盘」「机械硬盘」开头或只有接口名；取不到（虚拟机、网络路径）返回空，不报错。
func TestDiskMedia(t *testing.T) {
	m := DiskMedia(os.TempDir())
	t.Logf("临时目录所在磁盘：%q", m)
	if m != "" && !strings.HasPrefix(m, "固态硬盘") && !strings.HasPrefix(m, "机械硬盘") && strings.Contains(m, "，") {
		t.Fatalf("格式不对：%q", m)
	}
	if got := DiskMedia(`\\server\share\x`); got != "" {
		t.Fatalf("网络路径应返回空：%q", got)
	}
}
