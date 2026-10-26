package extract

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOCRModels(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "RapidOCR-json.exe")
	os.WriteFile(exe, []byte("x"), 0o755)
	os.MkdirAll(filepath.Join(dir, "models"), 0o755)
	c := OCRConfig{Command: exe, Args: []string{"--models=models", "--det=det.onnx", "--rec=rec_v4.onnx", "--keys=dict.txt", "--ensureAscii=1"}}
	if got := c.Models(); !slices.Equal(got, []string{OCRModelV4}) {
		t.Fatalf("只有自带模型时：%v", got)
	}
	if got := c.WithModel(OCRModelV6); !slices.Equal(got.Args, c.Args) {
		t.Fatalf("没有 v6 文件时应原样返回：%v", got.Args)
	}
	for _, f := range []string{"PP-OCRv6_rec_small.onnx", "PP-OCRv6_keys.txt"} {
		os.WriteFile(filepath.Join(dir, "models", f), []byte("x"), 0o644)
	}
	if got := c.Models(); !slices.Equal(got, []string{OCRModelV6, OCRModelV4}) {
		t.Fatalf("有 v6 时：%v", got)
	}
	want := []string{"--models=models", "--det=det.onnx", "--ensureAscii=1", "--rec=PP-OCRv6_rec_small.onnx", "--keys=PP-OCRv6_keys.txt"}
	if got := c.WithModel(OCRModelV6).Args; !slices.Equal(got, want) {
		t.Fatalf("换成 v6 的参数：%v", got)
	}
	if !slices.Equal(c.Args[2:4], []string{"--rec=rec_v4.onnx", "--keys=dict.txt"}) {
		t.Fatal("不应改动原来的参数")
	}
	if got := c.WithModel(OCRModelV6Q).Args; !slices.Equal(got, c.Args) {
		t.Fatalf("没有量化版文件时应原样返回：%v", got)
	}
}
