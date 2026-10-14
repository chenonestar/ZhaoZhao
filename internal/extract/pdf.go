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
		txt, err := d.text(i)
		if txt = strings.TrimSpace(txt); err != nil || txt == "" {
			blank = append(blank, i+1)
			continue
		}
		texts[i+1] = plainText(txt)
	}
	if n == 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "PDF 没有页面"}, true
	}
	var got pdfOCRResult
	var ocrErr error
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
		if err != nil || txt == "" {
			blank = append(blank, i) // 逐页判断：没有文字层的页视为扫描页
			continue
		}
		texts[i] = plainText(txt)
	}
	if n == 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "PDF 没有页面"}
	}
	var got pdfOCRResult
	var ocrErr error
	if len(blank) > 0 && opt.OCR.Enabled() {
		got, ocrErr = ocrPDFPages(p, blank, opt)
	}
	return pdfResult(n, texts, blank, got, ocrErr, opt)
}

// pdfResult 把各页文字（含 OCR 结果）按页码拼成提取结果。
func pdfResult(n int, texts map[int]string, blank []int, got pdfOCRResult, ocrErr error, opt Options) doc.Result {
	usedOCR := false
	ocrNote := got.note()
	for pg, t := range got.texts {
		if strings.TrimSpace(t) != "" {
			texts[pg] = strings.TrimSpace(t)
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
			return doc.Result{Status: doc.StatusFailed, Err: ocrErr.Error()}
		}
		if !opt.OCR.Enabled() {
			return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，需要 OCR", NeedOCR: needOCR}
		}
		if ocrNote != "" {
			return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，OCR 也没有识别到文字：" + ocrNote}
		}
		return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，OCR 也没有识别到文字"}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs, OCR: usedOCR, NeedOCR: needOCR, Err: ocrNote}
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
