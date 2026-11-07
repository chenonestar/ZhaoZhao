package extract

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestDescribeExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh 构造退出码")
	}
	err := exec.Command("sh", "-c", "echo boom >&2; exit 3").Run()
	msg := DescribeExit(err, "boom")
	if !strings.Contains(msg, "退出码 3") || !strings.Contains(msg, "boom") {
		t.Fatalf("got %q", msg)
	}
}

func TestCheckOCRMissing(t *testing.T) {
	st := CheckOCR(OCRConfig{})
	if st.OK || !strings.Contains(st.Message, "未找到") {
		t.Fatalf("got %+v", st)
	}
}
