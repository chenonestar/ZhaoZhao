// winocr 用找找里的 Windows 自带 OCR（extract 包直接调用 Windows.Media.Ocr）认目录里的 BMP，每页输出一行 JSON：
// {"file": 文件名, "ms": 识别用时（毫秒，不含读图）, "text": 识别出的文字}。打包时由 ocr_model_test.py 调用并算准确率。
// 系统没有中文识别语言时退出码为 3。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"golang.org/x/image/bmp"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：winocr 目录")
		os.Exit(2)
	}
	if err := extract.WinOCRAvailable(); err != nil {
		fmt.Println("不可用：" + err.Error())
		os.Exit(3)
	}
	files, _ := filepath.Glob(filepath.Join(os.Args[1], "*.bmp"))
	sort.Strings(files)
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			fail(err)
		}
		img, err := bmp.Decode(f)
		f.Close()
		if err != nil {
			fail(fmt.Errorf("%s：%v", p, err))
		}
		t0 := time.Now()
		text, err := extract.WinOCRImage(img)
		if err != nil {
			fail(fmt.Errorf("%s：%v", p, err))
		}
		b, _ := json.Marshal(map[string]any{"file": filepath.Base(p), "ms": time.Since(t0).Milliseconds(), "text": text})
		fmt.Println(string(b))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
