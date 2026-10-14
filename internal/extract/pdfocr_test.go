package extract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/scanpdf"
)

// 测试二进制自己充当 OCR 组件：返回「fakepage-图片文件名」，据此核对页码对应关系。
func TestMain(m *testing.M) {
	if os.Getenv("ZZ_FAKE_OCR") != "" {
		fmt.Println("OCR init completed.")
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			var req struct {
				ImagePath string `json:"image_path"`
			}
			json.Unmarshal(sc.Bytes(), &req)
			fmt.Printf(`{"code":100,"data":[{"text":"fakepage-%s"}]}`+"\n", strings.TrimSuffix(filepath.Base(req.ImagePath), filepath.Ext(req.ImagePath)))
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func grayPage(seed int) image.Image {
	img := image.NewGray(image.Rect(0, 0, 400, 300))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	// 几行「文字」（深色横条），渲染后不会被当作空白页
	for y := 40 + seed; y < 260; y += 30 {
		for x := 30; x < 370; x++ {
			for k := 0; k < 8; k++ {
				img.Set(x, y+k, color.Gray{0})
			}
		}
	}
	return img
}

// 扫描版 PDF 逐页识别：页码对应正确、逐页报告进度。没有 PDFium 时 JBIG2 页跳过并注明原因；
// 有 PDFium 时整页渲染后识别（这里的 JBIG2 数据是无法解码的假数据，渲染出空白页，不送去识别）。
func TestScannedPDFPageByPage(t *testing.T) {
	t.Setenv("ZZ_FAKE_OCR", "1")
	defer ShutdownOCR()
	var b bytes.Buffer
	pages := []scanpdf.Page{{Image: grayPage(1)}, {Image: grayPage(2)}, {JBIG2: true}, {Image: grayPage(4)}}
	if err := scanpdf.Write(&b, pages); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "扫描件.pdf")
	os.WriteFile(p, b.Bytes(), 0o644)

	opt := DefaultOptions()
	opt.OCR = OCRConfig{Command: os.Args[0], Args: []string{"-test.run=^$"}, MinSide: 50, PageSec: 30}
	var prog []string
	opt.SetProgress(func(s string) { prog = append(prog, s) })
	r := Extract(p, "pdf", opt)
	if r.Status != doc.StatusOK || !r.OCR {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"fakepage-p1", "fakepage-p2", "fakepage-p4"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("缺少 %s: %q", want, r.Text)
		}
	}
	_, noPDFium := PDFiumPath()
	if strings.Contains(r.Text, "fakepage-p3") || (noPDFium != nil && !strings.Contains(r.Err, "JBIG2")) {
		t.Fatalf("JBIG2 页应跳过: text=%q err=%q", r.Text, r.Err)
	}
	if len(r.Locs) != 3 || r.Locs[2].Label != "第4页" {
		t.Fatalf("页码定位: %+v", r.Locs)
	}
	if len(prog) < 4 || prog[len(prog)-1] != "第 4/4 页" {
		t.Fatalf("进度: %v", prog)
	}
}
