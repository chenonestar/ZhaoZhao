package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "image/gif"
	_ "image/jpeg"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// OCRConfig 配置 OCR 组件。组件是一个常驻子进程，使用 RapidOCR-json 的管道协议：
// 启动后打印 "OCR init completed."，之后每行输入 {"image_path": "..."}，每行输出
// {"code":100,"data":[{"text":"..."}]}。
type OCRConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	MinSide int      `json:"minSide"` // 短边小于该像素数的图片不做 OCR（默认 200）
	PageSec int      `json:"pageSec"` // 单页（单张图）OCR 超时秒数（默认 120）
}

// Enabled 说明是否配置了 OCR 组件。
func (c OCRConfig) Enabled() bool { return c.Command != "" }

// FindOCR 自动发现 OCR 组件：环境变量 ZHAOZHAO_OCR，其次安装目录下 components/ocr/，最后 PATH。
func FindOCR() OCRConfig {
	c := OCRConfig{MinSide: 200, PageSec: 120}
	if v := strings.TrimSpace(os.Getenv("ZHAOZHAO_OCR")); v != "" {
		f := strings.Fields(v)
		c.Command, c.Args = f[0], f[1:]
		return c
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, n := range []string{"RapidOCR-json.exe", "RapidOCR-json"} {
			p := filepath.Join(dir, "components", "ocr", n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				c.Command = p
				return c
			}
		}
	}
	if p, err := exec.LookPath("RapidOCR-json"); err == nil {
		c.Command = p
	}
	return c
}

// ---- 常驻 OCR 进程（每个解析子进程各自持有一个） ----

type ocrProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	key string
}

var (
	ocrMu   sync.Mutex
	ocrCur  *ocrProc
	ocrFail int
)

func (c OCRConfig) key() string { return c.Command + "\x00" + strings.Join(c.Args, "\x00") }

func startOCR(c OCRConfig) (*ocrProc, error) {
	cmd := exec.Command(c.Command, c.Args...)
	cmd.Dir = filepath.Dir(c.Command)
	if !filepath.IsAbs(c.Command) {
		cmd.Dir = ""
	}
	cmd.Stderr = io.Discard
	setProcAttr(cmd)
	cmd.Cancel = nil
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &ocrProc{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20), key: c.key()}
	// 等待初始化完成（加载模型，首次可能需要数秒）
	ready := make(chan error, 1)
	go func() {
		for {
			line, err := p.out.ReadString('\n')
			if err != nil {
				ready <- fmt.Errorf("OCR 组件启动失败: %v", err)
				return
			}
			if strings.Contains(line, "init completed") {
				ready <- nil
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			killOCR(p)
			return nil, err
		}
	case <-time.After(90 * time.Second):
		killOCR(p)
		return nil, fmt.Errorf("OCR 组件启动超时")
	}
	return p, nil
}

func killOCR(p *ocrProc) {
	if p == nil {
		return
	}
	p.in.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_, _ = p.cmd.Process.Wait()
}

type ocrResp struct {
	Code int             `json:"code"`
	Data json.RawMessage `json:"data"`
}

// ocrImage 识别一张图片，返回按阅读顺序拼接的文字。
func ocrImage(c OCRConfig, imgPath string) (string, error) {
	ocrMu.Lock()
	defer ocrMu.Unlock()
	if ocrCur == nil || ocrCur.key != c.key() {
		killOCR(ocrCur)
		ocrCur = nil
		p, err := startOCR(c)
		if err != nil {
			return "", err
		}
		ocrCur = p
	}
	p := ocrCur
	req, _ := json.Marshal(map[string]string{"image_path": imgPath})
	if _, err := p.in.Write(append(req, '\n')); err != nil {
		killOCR(p)
		ocrCur = nil
		return "", fmt.Errorf("OCR 组件已退出: %v", err)
	}
	sec := c.PageSec
	if sec <= 0 {
		sec = 120
	}
	type reply struct {
		text string
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		for {
			line, err := p.out.ReadBytes('\n')
			if err != nil {
				ch <- reply{err: err}
				return
			}
			var r ocrResp
			if json.Unmarshal(bytes.TrimSpace(line), &r) != nil {
				continue // 非协议行
			}
			switch r.Code {
			case 100:
				var items []struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(r.Data, &items); err != nil {
					ch <- reply{err: err}
					return
				}
				var b strings.Builder
				for _, it := range items {
					b.WriteString(it.Text)
					b.WriteByte('\n')
				}
				ch <- reply{text: b.String()}
			case 101:
				ch <- reply{}
			default:
				ch <- reply{err: fmt.Errorf("OCR 识别失败（代码 %d）: %s", r.Code, strings.Trim(string(r.Data), `"`))}
			}
			return
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sec)*time.Second)
	defer cancel()
	select {
	case r := <-ch:
		if r.err != nil && (strings.Contains(r.err.Error(), "EOF") || strings.Contains(r.err.Error(), "file already closed")) {
			killOCR(p)
			ocrCur = nil
		}
		return r.text, r.err
	case <-ctx.Done():
		killOCR(p)
		ocrCur = nil
		return "", fmt.Errorf("OCR 超时")
	}
}

// ---- 图片 ----

func minSide(c OCRConfig) int {
	if c.MinSide > 0 {
		return c.MinSide
	}
	return 200
}

// tiffPages 返回 TIFF 每一页的 IFD 偏移。
func tiffPages(b []byte) []uint32 {
	if len(b) < 8 {
		return nil
	}
	var bo binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil
	}
	if bo.Uint16(b[2:4]) != 42 {
		return nil
	}
	var pages []uint32
	off := bo.Uint32(b[4:8])
	seen := map[uint32]bool{}
	for off != 0 && int(off)+2 <= len(b) && !seen[off] && len(pages) < 2000 {
		seen[off] = true
		pages = append(pages, off)
		n := int(bo.Uint16(b[off : off+2]))
		next := int(off) + 2 + n*12
		if next+4 > len(b) {
			break
		}
		off = bo.Uint32(b[next : next+4])
	}
	return pages
}

// tiffPagePNG 把 TIFF 的第 k 页转成 PNG：把文件头里指向首个 IFD 的偏移改成第 k 页的 IFD，
// 所有数据偏移都是绝对的，因此标准解码器直接解出该页。
func tiffPagePNG(b []byte, ifd uint32) ([]byte, error) {
	cp := append([]byte(nil), b...)
	if string(cp[:2]) == "II" {
		binary.LittleEndian.PutUint32(cp[4:8], ifd)
	} else {
		binary.BigEndian.PutUint32(cp[4:8], ifd)
	}
	img, err := tiff.Decode(bytes.NewReader(cp))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// tempImage 把图片数据写成 ASCII 文件名的临时文件（OCR 组件对非 ASCII 路径支持不好）。
func tempImage(dir, name string, data []byte) (string, error) {
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, data, 0o600)
}

func extractImage(p, ext string, opt Options) doc.Result {
	if !opt.OCR.Enabled() {
		return doc.Result{Status: doc.StatusSkipped, Err: "OCR 组件未安装，仅索引文件名"}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	tmp, err := os.MkdirTemp("", "zhaozhao-ocr-")
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	defer os.RemoveAll(tmp)

	var b doc.Builder
	ocrOne := func(label string, img []byte, ext string) error {
		cfg, _, derr := image.DecodeConfig(bytes.NewReader(img))
		if derr == nil && (cfg.Width < minSide(opt.OCR) || cfg.Height < minSide(opt.OCR)) {
			return errTooSmall
		}
		path, err := tempImage(tmp, "img"+ext, img)
		if err != nil {
			return err
		}
		text, err := ocrImage(opt.OCR, path)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) != "" {
			if label != "" {
				b.Mark(label)
			}
			b.WriteString(text)
		}
		return nil
	}

	switch ext {
	case "tif", "tiff":
		pages := tiffPages(raw)
		if len(pages) == 0 {
			return doc.Result{Status: doc.StatusFailed, Err: "TIFF 文件已损坏"}
		}
		small := 0
		for i, off := range pages {
			pngData, err := tiffPagePNG(raw, off)
			if err != nil {
				return doc.Result{Status: doc.StatusFailed, Err: fmt.Sprintf("TIFF 第 %d 页无法解码: %v", i+1, err)}
			}
			err = ocrOne("第"+strconv.Itoa(i+1)+"页", pngData, ".png")
			if err == errTooSmall {
				small++
				continue
			}
			if err != nil {
				return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
			}
		}
		if small == len(pages) {
			return doc.Result{Status: doc.StatusSkipped, Err: "图片尺寸过小，已跳过 OCR"}
		}
	case "webp", "bmp", "gif":
		// OCR 组件不一定支持这些格式：统一转成 PNG
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: "图片无法解码: " + err.Error()}
		}
		var out bytes.Buffer
		_ = png.Encode(&out, img)
		if err := ocrOne("", out.Bytes(), ".png"); err != nil {
			return imageErr(err)
		}
	default:
		e := "." + ext
		if ext == "jpeg" {
			e = ".jpg"
		}
		if err := ocrOne("", raw, e); err != nil {
			return imageErr(err)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return doc.Result{Status: doc.StatusNoContent, Err: "没有识别到文字", OCR: true}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs, OCR: true}
}

var errTooSmall = fmt.Errorf("图片尺寸过小，已跳过 OCR")

func imageErr(err error) doc.Result {
	if err == errTooSmall {
		return doc.Result{Status: doc.StatusSkipped, Err: err.Error()}
	}
	return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
}

// ---- 扫描版 PDF ----

func decodeConfig(b []byte, fileType string) (image.Config, error) {
	if t := strings.ToLower(fileType); t == "tif" || t == "tiff" {
		return tiff.DecodeConfig(bytes.NewReader(b))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	return cfg, err
}

// ocrPDFPages 对没有文字层的页取出内嵌图片并做 OCR，返回页码 → 文字。
func ocrPDFPages(p string, pages []int, opt Options) (map[int]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sel := make([]string, len(pages))
	for i, n := range pages {
		sel[i] = strconv.Itoa(n)
	}
	conf := model.NewDefaultConfiguration()
	imgs, err := api.ExtractImagesRaw(context.Background(), f, sel, conf)
	if err != nil {
		return nil, fmt.Errorf("无法取出页面图片: %v", err)
	}
	tmp, err := os.MkdirTemp("", "zhaozhao-pdfocr-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	best := map[int]model.Image{}
	data := map[int][]byte{}
	for _, m := range imgs {
		for _, im := range m {
			if im.Thumb {
				continue
			}
			b, err := io.ReadAll(im)
			if err != nil || len(b) == 0 {
				continue
			}
			w, h := im.Width, im.Height
			if w == 0 || h == 0 { // 原始提取不一定带尺寸，读图片头获取
				if cfg, err := decodeConfig(b, im.FileType); err == nil {
					w, h = cfg.Width, cfg.Height
				}
			}
			if w < minSide(opt.OCR) || h < minSide(opt.OCR) {
				continue
			}
			if cur, ok := best[im.PageNr]; ok && cur.Width*cur.Height >= w*h {
				continue
			}
			im.Width, im.Height = w, h
			best[im.PageNr] = im
			data[im.PageNr] = b
		}
	}
	out := map[int]string{}
	for _, pg := range pages {
		im, ok := best[pg]
		if !ok {
			continue
		}
		b := data[pg]
		ext := "." + strings.ToLower(im.FileType)
		switch strings.ToLower(im.FileType) {
		case "jpg", "jpeg", "png":
		case "tif", "tiff":
			if pages := tiffPages(b); len(pages) > 0 {
				if pngData, err := tiffPagePNG(b, pages[0]); err == nil {
					b, ext = pngData, ".png"
				}
			}
		default:
			continue // jp2 等 OCR 组件不支持的格式
		}
		path, err := tempImage(tmp, "p"+strconv.Itoa(pg)+ext, b)
		if err != nil {
			return out, err
		}
		text, err := ocrImage(opt.OCR, path)
		if err != nil {
			return out, err
		}
		out[pg] = text
	}
	return out, nil
}
