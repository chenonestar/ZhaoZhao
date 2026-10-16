package extract

import (
	"strings"
	"testing"
)

// 指定线程数时追加 RapidOCR 的 --numThread；线程数不同的 OCR 进程不能共用（key 不同，会重新启动）。
func TestOCRThreadsArg(t *testing.T) {
	c := OCRConfig{Command: "ocr", Args: []string{"--models=models"}}
	if got := strings.Join(c.args(), " "); got != "--models=models" {
		t.Fatal(got)
	}
	c4 := c
	c4.Threads = 4
	if got := strings.Join(c4.args(), " "); got != "--models=models --numThread=4" {
		t.Fatal(got)
	}
	if c.key() == c4.key() || len(c.Args) != 1 {
		t.Fatal("线程数不同应是不同的 OCR 进程，且不能改动原参数")
	}
}
