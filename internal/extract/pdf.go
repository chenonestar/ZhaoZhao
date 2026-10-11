package extract

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/ledongthuc/pdf"
)

// extractPDF 按页提取文字层。没有文字的页视为扫描页：整份文件都没有文字时
// 返回 StatusNoContent，交给 OCR 处理。
func extractPDF(p string) (res doc.Result) {
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
	var b doc.Builder
	empty := 0
	n := r.NumPage()
	for i := 1; i <= n; i++ {
		page := r.Page(i)
		if page.V.IsNull() {
			continue
		}
		txt, err := safePageText(page)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "password") {
				return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}
			}
			empty++
			continue
		}
		txt = strings.TrimSpace(txt)
		if txt == "" {
			empty++
			continue
		}
		b.Mark("第" + strconv.Itoa(i) + "页")
		b.Line(plainText(txt))
	}
	if b.Len() == 0 {
		if n == 0 {
			return doc.Result{Status: doc.StatusFailed, Err: "PDF 没有页面"}
		}
		return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，需要 OCR"}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}
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
