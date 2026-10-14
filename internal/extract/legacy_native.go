package extract

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/richardlehane/mscfb"
)

// 老格式的处理方式（Options.LegacyMode）。
const (
	LegacyAuto   = ""       // 先用内置解析，处理不了再用 LibreOffice
	LegacyNative = "native" // 只用内置解析；处理不了时返回 NeedConvert，由主程序排到老格式转换队列
	LegacyLO     = "lo"     // 只用 LibreOffice 转换
)

// legacyFamily 是扩展名对应的文档类别：文字、表格、演示。
var legacyFamily = map[string]string{
	"doc": "doc", "dot": "doc", "wps": "doc", "wpt": "doc", "rtf": "doc", "docx": "doc",
	"xls": "xls", "xlt": "xls", "et": "xls", "ett": "xls", "xlsx": "xls",
	"ppt": "ppt", "pps": "ppt", "pot": "ppt", "dps": "ppt", "dpt": "ppt", "pptx": "ppt",
}

func extractLegacyAny(p, ext string, opt Options) doc.Result {
	if opt.LegacyMode == LegacyLO {
		return extractLegacy(p, ext, opt)
	}
	r, why := extractLegacyNative(p, ext, opt)
	if why == "" {
		return r
	}
	if opt.LegacyMode == LegacyNative {
		return doc.Result{Status: doc.StatusFailed, Err: "内置解析不支持：" + why, NeedConvert: true}
	}
	logx.Debugf("内置解析不支持 %s（%s），改用 LibreOffice", logx.P(p), why)
	return extractLegacy(p, ext, opt)
}

// extractLegacyNative 按文件的实际内容（而不是扩展名）选用内置解析器：扩展名经常不可信，
// .doc 可能是 RTF 或网页，.xls 可能是网页表格，也可能是改了扩展名的 docx/xlsx。
// why 非空表示内置解析器处理不了，需要 LibreOffice。
func extractLegacyNative(p, ext string, opt Options) (res doc.Result, why string) {
	defer func() {
		if r := recover(); r != nil {
			res, why = doc.Result{}, fmt.Sprintf("内置解析异常: %v", r)
		}
	}()
	head, err := readHead(p, 512)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: "读取文件失败: " + err.Error()}, ""
	}
	fam := legacyFamily[ext]
	var (
		r    doc.Result
		nerr error
	)
	switch {
	case len(head) == 0:
		return doc.Result{Status: doc.StatusNoContent, Err: "空文件"}, ""
	case bytes.HasPrefix(head, oleMagic):
		names, err := oleRootNames(p)
		if err != nil {
			return doc.Result{}, err.Error()
		}
		switch {
		case names["EncryptedPackage"]: // 加密的 docx/xlsx/pptx
			return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}, ""
		case names["WordDocument"]:
			r, nerr = extractDocNative(p)
		case names["Workbook"] || names["Book"]:
			r, nerr = extractXlsNative(p)
		case names["PowerPoint Document"]:
			r, nerr = extractPptNative(p)
		default:
			return doc.Result{}, "不认识的 OLE 文档"
		}
	case isRTF(head):
		r = extractRTF(p, opt)
	case bytes.HasPrefix(head, []byte("PK\x03\x04")): // 改了扩展名的 docx/xlsx/pptx
		switch fam {
		case "doc":
			r = extractDocx(p)
		case "xls":
			r = extractXlsx(p)
		default:
			r = extractPptx(p)
		}
		if r.Status == doc.StatusFailed {
			return doc.Result{}, "ZIP 格式但不是对应的 Office 文档"
		}
	case looksHTML(head): // 「另存为网页」后又改成 .doc/.xls 的文件
		raw, err := readLimited(p, opt.MaxTextBytes)
		if err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}, ""
		}
		return doc.Result{Status: doc.StatusOK, Text: htmlText(raw), LineBased: true}, ""
	default:
		return doc.Result{}, "无法识别的格式"
	}
	var en errNative
	switch {
	case nerr == nil:
		return r, ""
	case errors.Is(nerr, errNativeEncrypted):
		return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}, ""
	case errors.As(nerr, &en):
		return doc.Result{}, en.why
	}
	return doc.Result{Status: doc.StatusFailed, Err: nerr.Error()}, ""
}

// oleRootNames 返回 OLE 复合文档根目录下的流名称（只读目录，不读流内容）。
func oleRootNames(p string) (map[string]bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d, err := mscfb.New(f)
	if err != nil {
		return nil, nativeUnsupported("不是有效的 OLE 复合文档: %v", err)
	}
	names := map[string]bool{}
	for _, e := range d.File {
		if len(e.Path) == 0 {
			names[e.Name] = true
		}
	}
	return names, nil
}

func looksHTML(head []byte) bool {
	h := bytes.ToLower(bytes.TrimLeft(head, " \t\r\n\xef\xbb\xbf"))
	return bytes.HasPrefix(h, []byte("<html")) || bytes.HasPrefix(h, []byte("<!doctype html")) ||
		(bytes.HasPrefix(h, []byte("<")) && bytes.Contains(h, []byte("<html")) && !bytes.HasPrefix(h, []byte("<?xml")))
}
