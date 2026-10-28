package extract

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/ledongthuc/pdf"
)

// extractPDF 按页提取文字层。没有文字的页视为扫描页：开启 OCR 时逐页识别；
// 整份文件都没有文字时返回 StatusNoContent，交给 OCR 处理。
// 有 PDFium 时用它（文字提取更准确，扫描页直接渲染成图片，任何图片格式都能识别），否则用纯 Go 解析器。
func extractPDF(p string, opt Options) doc.Result {
	if _, err := PDFiumPath(); err == nil {
		if r, ok := extractPDFium(p, opt); ok {
			return r
		}
	}
	return extractPDFGo(p, opt)
}

// extractPDFium 用 PDFium 提取。ok 为 false 表示 PDFium 打不开这个文件，应改用纯 Go 解析器。
func extractPDFium(p string, opt Options) (res doc.Result, ok bool) {
	pdfiumMu.Lock()
	defer pdfiumMu.Unlock()
	d, err := openPDFium(p)
	if err == errPDFPassword {
		return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}, true
	}
	if err != nil {
		logx.Debugf("PDFium 无法打开 %s（%v），改用内置解析器", logx.P(p), err)
		return doc.Result{}, false
	}
	defer d.close()
	n := d.pages()
	texts := make(map[int]string, n)
	var blank []int
	for i := 0; i < n; i++ {
		if n > 50 && i%50 == 0 {
			opt.Progress("读取文字 %d/%d 页", i+1, n)
		}
		txt, cov, err := d.page(i, true)
		txt = strings.TrimSpace(txt)
		act := pageOCR
		if err == nil {
			act = classifyPage(txt, cov)
		}
		switch act {
		case pageOCR:
			if txt != "" {
				logx.Debugf("PDF %s 第 %d 页的文字层像乱码，改用 OCR", logx.P(p), i+1)
			}
			blank = append(blank, i+1)
		case pageOCRKeepText:
			logx.Debugf("PDF %s 第 %d 页只有 %d 个字、图片占 %.0f%%，按扫描页识别", logx.P(p), i+1, len([]rune(txt)), cov*100)
			blank = append(blank, i+1)
			texts[i+1] = plainText(txt)
		default:
			texts[i+1] = plainText(txt)
		}
	}
	if n == 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "PDF 没有页面"}, true
	}
	var got pdfOCRResult
	var ocrErr error
	blank = limitPages(blank, opt)
	if len(blank) > 0 && opt.OCR.Enabled() {
		got, ocrErr = ocrPDFiumPages(d, p, blank, opt)
	}
	return pdfResult(n, texts, blank, got, ocrErr, opt), true
}

// extractPDFGo 用纯 Go 解析器提取（没有 PDFium，或 PDFium 打不开时）。
func extractPDFGo(p string, opt Options) (res doc.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = doc.Result{Status: doc.StatusFailed, Err: fmt.Sprintf("PDF 解析异常: %v", r)}
		}
	}()
	f, r, err := pdf.Open(p)
	if err != nil {
		return pdfErr(err)
	}
	defer f.Close()
	if r.Trailer().Key("Encrypt").Kind() != pdf.Null {
		// 只有空密码能解开的文件库会照常读取；读不出内容时按加密处理
		defer func() {
			if res.Status != doc.StatusOK {
				res = doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}
			}
		}()
	}
	n := r.NumPage()
	texts := make(map[int]string, n)
	var blank []int
	for i := 1; i <= n; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		txt, err := safePageText(page)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "password") {
			return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}
		}
		txt = strings.TrimSpace(txt)
		if err != nil || classifyPage(txt, 0) == pageOCR {
			blank = append(blank, i) // 逐页判断：没有文字层（或文字层是乱码）的页视为扫描页
			continue
		}
		texts[i] = plainText(txt)
	}
	if n == 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "PDF 没有页面"}
	}
	var got pdfOCRResult
	var ocrErr error
	blank = limitPages(blank, opt)
	if len(blank) > 0 && opt.OCR.Enabled() {
		got, ocrErr = ocrPDFPages(p, blank, opt)
	}
	return pdfResult(n, texts, blank, got, ocrErr, opt)
}

// limitPages：设了 OCRPageLimit 时只留前几个扫描页。
func limitPages(blank []int, opt Options) []int {
	if opt.OCRPageLimit > 0 && opt.OCR.Enabled() && len(blank) > opt.OCRPageLimit {
		return blank[:opt.OCRPageLimit]
	}
	return blank
}

// pdfResult 把各页文字（含 OCR 结果）按页码拼成提取结果。
func pdfResult(n int, texts map[int]string, blank []int, got pdfOCRResult, ocrErr error, opt Options) doc.Result {
	usedOCR := false
	ocrNote := got.note()
	for pg, t := range got.texts {
		if t = strings.TrimSpace(t); t != "" {
			if old, ok := texts[pg]; ok { // 扫描页上原有的少量文字（页码、电子章等）也保留
				t = old + "\n" + t
			}
			texts[pg] = t
			usedOCR = true
		}
	}
	var b doc.Builder
	for i := 1; i <= n; i++ {
		if t, ok := texts[i]; ok {
			b.Mark("第" + strconv.Itoa(i) + "页")
			b.Line(t)
		}
	}
	needOCR := len(blank) > 0 && !opt.OCR.Enabled()
	if b.Len() == 0 {
		if ocrErr != nil {
			return doc.Result{Status: doc.StatusFailed, Err: ocrErr.Error(), OCRPages: len(blank)}
		}
		if !opt.OCR.Enabled() {
			return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，需要 OCR", NeedOCR: needOCR, OCRPages: len(blank)}
		}
		if ocrNote != "" {
			return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，OCR 也没有识别到文字：" + ocrNote, OCRPages: len(blank)}
		}
		return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，OCR 也没有识别到文字", OCRPages: len(blank)}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs, OCR: usedOCR, NeedOCR: needOCR, Err: ocrNote, OCRPages: len(blank)}
}

func safePageText(p pdf.Page) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return p.GetPlainText(nil)
}

func pdfErr(err error) doc.Result {
	msg := err.Error()
	if err == pdf.ErrInvalidPassword || strings.Contains(strings.ToLower(msg), "password") || strings.Contains(strings.ToLower(msg), "encrypt") {
		return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}
	}
	return doc.Result{Status: doc.StatusFailed, Err: "PDF 已损坏或无法解析: " + msg}
}
