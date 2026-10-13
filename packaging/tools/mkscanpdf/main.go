// mkscanpdf 把若干张图片合成一份「扫描版」PDF（每页一张图、没有文字层），供冒烟测试使用。
//
// 用法: go run ./packaging/tools/mkscanpdf out.pdf page1.png page2.png ...
package main

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"

	"github.com/chenonestar/zhaozhao/internal/scanpdf"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: mkscanpdf out.pdf page1.png [page2.png ...]")
		os.Exit(2)
	}
	var pages []scanpdf.Page
	for _, p := range os.Args[2:] {
		f, err := os.Open(p)
		if err != nil {
			fail(err)
		}
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			fail(fmt.Errorf("%s: %w", p, err))
		}
		pages = append(pages, scanpdf.Page{Image: img})
	}
	out, err := os.Create(os.Args[1])
	if err != nil {
		fail(err)
	}
	if err := scanpdf.Write(out, pages); err != nil {
		fail(err)
	}
	if err := out.Close(); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
