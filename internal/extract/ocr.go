package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenonestar/zhaozhao/internal/awake"
	"image"
	"image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "image/gif"
	_ "image/jpeg"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
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
	// Threads 是每个 OCR 进程用的线程数（RapidOCR 的 --numThread，它默认 4）。0 表示不指定。
	// 多个 OCR 进程的线程总数超过 CPU 核心数时互相争抢，总速度反而下降（实测可慢一倍）。
	Threads int `json:"threads,omitempty"`
}

// args 返回启动 OCR 进程的参数。
func (c OCRConfig) args() []string {
	if c.Threads <= 0 {
		return c.Args
	}
	return append(append([]string{}, c.Args...), fmt.Sprintf("--numThread=%d", c.Threads))
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
		dir := filepath.Join(filepath.Dir(exe), "components", "ocr")
		for _, n := range []string{"RapidOCR_json.exe", "RapidOCR-json.exe", "RapidOCR_json", "RapidOCR-json"} {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				c.Command = p
				c.Args = readArgsFile(filepath.Join(dir, "args.txt"))
				return c
			}
		}
	}
	for _, n := range []string{"RapidOCR_json", "RapidOCR-json"} {
		if p, err := exec.LookPath(n); err == nil {
			c.Command = p
			return c
		}
	}
	return c
}

// readArgsFile 读取组件目录里的 args.txt：每行一个启动参数（如 --models=models），# 开头为注释。
// 发行包用它指定识别语言所用的模型文件。
func readArgsFile(p string) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// ocrRequest 生成一行 OCR 请求。非 ASCII 字符一律写成 \uXXXX，反斜杠换成斜杠：
// 组件按系统代码页读取命令行和管道，中文路径（如 C:\Users\张三\…）会因此出错。
func ocrRequest(imgPath string) []byte {
	var b strings.Builder
	b.WriteString(`{"image_path":"`)
	for _, r := range filepath.ToSlash(imgPath) {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	b.WriteString(`"}`)
	b.WriteByte('\n')
	return []byte(b.String())
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

func (c OCRConfig) key() string { return c.Command + "\x00" + strings.Join(c.args(), "\x00") }

func startOCR(c OCRConfig) (*ocrProc, error) {
	cmd := exec.Command(c.Command, c.args()...)
	cmd.Dir = filepath.Dir(c.Command)
	if !filepath.IsAbs(c.Command) {
		cmd.Dir = ""
	}
	errBuf := &tailBuffer{max: 2048}
	cmd.Stderr = errBuf
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
	t0 := time.Now()
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
	startTimer := awake.NewTimer(90 * time.Second) // 只计醒着的时间
	defer startTimer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			// 进程在初始化完成前就退出了：取退出码说明原因（例如缺少运行库）
			p.in.Close()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var werr error
			select {
			case werr = <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				werr = <-done
			}
			if werr == nil {
				werr = err
			}
			return nil, fmt.Errorf("OCR 组件启动失败：%s", DescribeExit(werr, errBuf.String()))
		}
	case <-startTimer.C:
		killOCR(p)
		return nil, fmt.Errorf("OCR 组件启动超时（90 秒内没有完成初始化）%s", suffix(errBuf.String()))
	}
	logx.Debugf("OCR 组件已启动（进程 %d，加载模型用时 %.1fs）", cmd.Process.Pid, time.Since(t0).Seconds())
	return p, nil
}

func suffix(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return "：" + truncate(s, 300)
	}
	return ""
}

// tailBuffer 只保留最后 max 个字节的输出，用于错误诊断。
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.b), "?")
}

// ShutdownOCR 结束常驻的 OCR 子进程。解析子进程退出前调用，避免留下孤儿进程。
func ShutdownOCR() {
	ocrMu.Lock()
	defer ocrMu.Unlock()
	killOCR(ocrCur)
	ocrCur = nil
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
	text, err := recognizeWith(p, c, imgPath)
	if err != nil && (errors.Is(err, errOCRDead) || errors.Is(err, errOCRTimeout)) {
		log.Printf("OCR 组件（进程 %d）%v，结束后重新启动", p.cmd.Process.Pid, err)
		killOCR(p) // 进程已退出或卡住：下次重新启动
		ocrCur = nil
	}
	return text, err
}

var (
	errOCRDead    = errors.New("OCR 组件已退出")
	errOCRTimeout = errors.New("OCR 超时")
)

// recognizeWith 用一个已启动的 OCR 进程识别一张图片。
func recognizeWith(p *ocrProc, c OCRConfig, imgPath string) (string, error) {
	if _, err := p.in.Write(ocrRequest(imgPath)); err != nil {
		return "", fmt.Errorf("%w: %v", errOCRDead, err)
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
				ch <- reply{err: fmt.Errorf("%w: %v", errOCRDead, err)}
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
	tm := awake.NewTimer(time.Duration(sec) * time.Second) // 只计醒着的时间
	defer tm.Stop()
	select {
	case r := <-ch:
		return r.text, r.err
	case <-tm.C:
		return "", fmt.Errorf("%w（单张图片超过 %d 秒）", errOCRTimeout, sec)
	}
}

// ---- 图片 ----

// ImageTooSmall 只读图片的文件头，判断是否太小、不值得 OCR（与识别时的规则相同：宽或高小于 MinSide，
// 多为图标、缩略图）。扫描时就排除，不进 OCR 队列。读不出尺寸时返回 false（交给识别时再判断）。
func ImageTooSmall(p string, c OCRConfig) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(bufio.NewReaderSize(f, 64<<10))
	return err == nil && (cfg.Width < minSide(c) || cfg.Height < minSide(c))
}

// ErrTooSmall 是图片太小时的说明（扫描时排除和识别时跳过用同一句话，失败列表里归为一类）。
const ErrTooSmall = "图片尺寸过小，已跳过 OCR"

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
		t1 := time.Now()
		text, err := ocrImage(opt.OCR, path)
		if err != nil {
			return err
		}
		logx.Debugf("OCR %s %s用时 %.1fs，识别出 %d 字", logx.P(p), label, time.Since(t1).Seconds(), len([]rune(text)))
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
			opt.Progress("第 %d/%d 页", i+1, len(pages))
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
			return doc.Result{Status: doc.StatusSkipped, Err: ErrTooSmall}
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

var errTooSmall = errors.New(ErrTooSmall)

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

// pdfOCRResult 是扫描版 PDF 的 OCR 结果。
type pdfOCRResult struct {
	texts   map[int]string // 页码 → 文字
	skipped map[string]int // 无法识别的页：原因 → 页数
}

func (r pdfOCRResult) note() string {
	if len(r.skipped) == 0 {
		return ""
	}
	var parts []string
	for why, n := range r.skipped {
		parts = append(parts, fmt.Sprintf("%d 页%s", n, why))
	}
	sort.Strings(parts)
	return strings.Join(parts, "，")
}

// ocrPDFiumPages 用 PDFium 把没有文字层的页逐页渲染成图片并做 OCR：不管页面里是什么格式的图片
// （JPEG、JBIG2、JPEG 2000、CCITT），也不管是一张图还是多张拼成，都按整页识别。
// 几乎全白的页（空白页）不送去识别。调用方持有 pdfiumMu。
func ocrPDFiumPages(d *pdfDoc, p string, pages []int, opt Options) (pdfOCRResult, error) {
	res := pdfOCRResult{texts: map[int]string{}, skipped: map[string]int{}}
	tmp, err := os.MkdirTemp("", "zhaozhao-pdfocr-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	skip := func(pg int, why string) {
		res.skipped[why]++
		logx.Debugf("PDF %s 第 %d 页%s，跳过", logx.P(p), pg, why)
	}
	logx.Debugf("PDF %s：共 %d 页，其中 %d 页没有文字层，渲染后 OCR", logx.P(p), d.pages(), len(pages))
	for i, pg := range pages {
		opt.Progress("第 %d/%d 页", i+1, len(pages))
		t0 := time.Now()
		img, err := d.render(pg-1, 200, 3500)
		if err != nil {
			skip(pg, "无法渲染（"+truncate(err.Error(), 80)+"）")
			continue
		}
		if blankImage(img) {
			continue // 空白页
		}
		path := filepath.Join(tmp, "p"+strconv.Itoa(pg)+".png")
		if err := writePNG(path, img); err != nil {
			return res, err
		}
		sz := img.Rect.Size()
		img = nil
		t1 := time.Now()
		text, err := ocrImage(opt.OCR, path)
		os.Remove(path)
		if errors.Is(err, errOCRTimeout) {
			skip(pg, "识别超时")
			continue
		}
		if err != nil {
			return res, err
		}
		logx.Debugf("OCR %s 第 %d 页（%d/%d）渲染 %.1fs、识别 %.1fs，图片 %dx%d，识别出 %d 字", logx.P(p), pg, i+1, len(pages),
			t1.Sub(t0).Seconds(), time.Since(t1).Seconds(), sz.X, sz.Y, len([]rune(text)))
		res.texts[pg] = text
	}
	if n := res.note(); n != "" {
		log.Printf("PDF %s：%s", logx.P(p), n)
	}
	return res, nil
}

// blankImage 判断渲染出的页面是否几乎全白（深色像素不到万分之五）。
func blankImage(g *image.Gray) bool {
	dark, total := 0, len(g.Pix)
	limit := total / 2000
	for _, v := range g.Pix {
		if v < 160 {
			if dark++; dark > limit {
				return false
			}
		}
	}
	return true
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ocrPDFPages 对没有文字层的页逐页取出内嵌图片并做 OCR（没有 PDFium 时）：PDF 结构只读一次，
// 每页取图、识别、删除临时文件后再处理下一页，内存里同时只有一页的图片。
// 某一页识别超时只跳过这一页；OCR 组件无法启动等错误才让整份文件失败。
func ocrPDFPages(p string, pages []int, opt Options) (pdfOCRResult, error) {
	res := pdfOCRResult{texts: map[int]string{}, skipped: map[string]int{}}
	f, err := os.Open(p)
	if err != nil {
		return res, err
	}
	defer f.Close()
	t0 := time.Now()
	opt.Progress("读取 PDF 结构")
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	ctx, err := api.ReadValidateAndOptimize(context.Background(), f, conf, nil)
	if err != nil {
		return res, fmt.Errorf("无法取出页面图片: %v", err)
	}
	logx.Debugf("PDF %s：读取结构用时 %.1fs，共 %d 页，其中 %d 页没有文字层需要 OCR", logx.P(p), time.Since(t0).Seconds(), ctx.PageCount, len(pages))
	tmp, err := os.MkdirTemp("", "zhaozhao-pdfocr-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	skip := func(pg int, why string) {
		res.skipped[why]++
		logx.Debugf("PDF %s 第 %d 页%s，跳过", logx.P(p), pg, why)
	}
	for i, pg := range pages {
		opt.Progress("第 %d/%d 页", i+1, len(pages))
		imgs, err := pdfcpu.ExtractPageImages(context.Background(), ctx, pg, false)
		if err != nil && len(imgs) == 0 {
			skip(pg, "取不出图片（"+truncate(err.Error(), 80)+"）")
			continue
		}
		// 一页可能有多张图（水印、印章、分条扫描）：取面积最大的一张
		var best *model.Image
		for k := range imgs {
			im := imgs[k]
			if im.Thumb {
				continue
			}
			if best == nil || im.Width*im.Height > best.Width*best.Height {
				best = &im
			}
		}
		if best == nil {
			skip(pg, "没有图片")
			continue
		}
		b, err := io.ReadAll(best)
		if err != nil || len(b) == 0 {
			skip(pg, "图片无法读取")
			continue
		}
		ext := "." + strings.ToLower(best.FileType)
		switch strings.ToLower(best.FileType) {
		case "jpg", "jpeg", "png":
		case "tif", "tiff":
			if tp := tiffPages(b); len(tp) > 0 {
				if pngData, err := tiffPagePNG(b, tp[0]); err == nil {
					b, ext = pngData, ".png"
				}
			}
		case "jbig2":
			skip(pg, "是 JBIG2 格式的黑白扫描图，暂不支持识别")
			continue
		case "jpx":
			skip(pg, "是 JPEG 2000 格式的扫描图，暂不支持识别")
			continue
		default:
			skip(pg, "的图片格式（"+best.FileType+"）暂不支持识别")
			continue
		}
		w, h := best.Width, best.Height
		if w == 0 || h == 0 {
			if cfg, err := decodeConfig(b, best.FileType); err == nil {
				w, h = cfg.Width, cfg.Height
			}
		}
		if w < minSide(opt.OCR) || h < minSide(opt.OCR) {
			skip(pg, "图片太小")
			continue
		}
		path, err := tempImage(tmp, "p"+strconv.Itoa(pg)+ext, b)
		b = nil
		if err != nil {
			return res, err
		}
		t1 := time.Now()
		text, err := ocrImage(opt.OCR, path)
		os.Remove(path)
		if errors.Is(err, errOCRTimeout) {
			skip(pg, "识别超时")
			continue
		}
		if err != nil {
			return res, err
		}
		logx.Debugf("OCR %s 第 %d 页（%d/%d）用时 %.1fs，图片 %dx%d，识别出 %d 字", logx.P(p), pg, i+1, len(pages), time.Since(t1).Seconds(), w, h, len([]rune(text)))
		res.texts[pg] = text
	}
	if n := res.note(); n != "" {
		log.Printf("PDF %s：%s", logx.P(p), n)
	}
	return res, nil
}
