// Package extract 从各种格式的文件中取出纯文本。
package extract

import (
	"path/filepath"
	"sort"
	"strings"
)

// 文件类别，对应搜索语法里 type: 的取值。
const (
	KindDoc     = "doc"
	KindSheet   = "sheet"
	KindSlide   = "slide"
	KindPDF     = "pdf"
	KindImage   = "image"
	KindText    = "text"
	KindCode    = "code"
	KindArchive = "archive"
	KindMail    = "mail"
	KindOther   = "other"
)

type handler int

const (
	hNone handler = iota
	hText
	hHTML
	hDocx
	hXlsx
	hPptx
	hODF
	hPDF
	hLegacy // 老格式，经 LibreOffice 转换
	hImage
	hZip
	hEML
	hMbox
	hMSG
	hXML // 只取 XML 里的文字（任免审批表 .lrmx 等）
)

type fmtInfo struct {
	kind string
	h    handler
}

var formats = map[string]fmtInfo{}

func reg(kind string, h handler, exts ...string) {
	for _, e := range exts {
		formats[e] = fmtInfo{kind, h}
	}
}

func init() {
	reg(KindText, hText, "txt", "md", "markdown", "log", "csv", "tsv", "json", "xml", "yaml", "yml", "ini", "conf",
		"cfg", "toml", "properties", "tex", "srt", "rst", "nfo", "reg", "env", "gitignore", "lrc", "vtt", "ass")
	reg(KindCode, hText, "go", "py", "js", "mjs", "ts", "jsx", "tsx", "java", "c", "cc", "cpp", "cxx", "h", "hpp", "cs",
		"rs", "rb", "php", "swift", "kt", "kts", "scala", "sh", "bash", "zsh", "bat", "cmd", "ps1", "vue", "css", "scss",
		"less", "lua", "pl", "r", "m", "mm", "dart", "sql", "gradle", "proto", "vb", "vbs", "asm", "s", "svelte", "jsp", "asp", "aspx")
	reg(KindText, hHTML, "html", "htm", "xhtml")
	reg(KindDoc, hXML, "lrmx") // 干部任免审批表（XML，照片以 base64 嵌在里面）
	reg(KindDoc, hDocx, "docx", "docm", "dotx", "dotm")
	reg(KindSheet, hXlsx, "xlsx", "xlsm", "xltx", "xltm")
	reg(KindSlide, hPptx, "pptx", "pptm", "potx", "potm", "ppsx", "ppsm")
	reg(KindDoc, hODF, "odt")
	reg(KindSheet, hODF, "ods")
	reg(KindSlide, hODF, "odp")
	reg(KindDoc, hLegacy, "doc", "dot", "wps", "wpt", "rtf")
	reg(KindSheet, hLegacy, "xls", "xlt", "et", "ett")
	reg(KindSlide, hLegacy, "ppt", "pps", "pot", "dps", "dpt")
	reg(KindPDF, hPDF, "pdf")
	reg(KindImage, hImage, "jpg", "jpeg", "png", "bmp", "tif", "tiff", "webp")
	reg(KindArchive, hZip, "zip", "7z", "rar", "tar", "gz", "tgz")
	reg(KindMail, hEML, "eml")
	reg(KindMail, hMbox, "mbox")
	reg(KindMail, hMSG, "msg")
	reg(KindMail, hNone, "pst", "ost")
}

// Ext 返回小写扩展名（不含点）。
func Ext(name string) string {
	e := strings.ToLower(filepath.Ext(name))
	return strings.TrimPrefix(e, ".")
}

// KindOf 返回扩展名对应的文件类别；未知返回 KindOther。
func KindOf(ext string) string {
	if f, ok := formats[ext]; ok {
		return f.kind
	}
	return KindOther
}

// Supported 说明扩展名是否有对应的内容提取器。
func Supported(ext string) bool {
	f, ok := formats[ext]
	return ok && f.h != hNone
}

// IsLegacy 是否为需要转换组件的老格式（优先级较低）。
func IsLegacy(ext string) bool { return formats[ext].h == hLegacy }

// OOXMLExts 返回全部 Office 2007 以后格式（docx/xlsx/pptx 及模板、带宏的变体）的扩展名。
func OOXMLExts() []string {
	var out []string
	for e, f := range formats {
		if f.h == hDocx || f.h == hXlsx || f.h == hPptx {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// LegacyExts 返回全部老格式扩展名。
func LegacyExts() []string {
	var out []string
	for e, f := range formats {
		if f.h == hLegacy {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// IsImage 是否为图片（需要 OCR）。
func IsImage(ext string) bool { return formats[ext].h == hImage }

// IsContainer 是否为压缩包或邮件容器。
func IsContainer(ext string) bool {
	switch formats[ext].h {
	case hZip, hEML, hMbox, hMSG:
		return true
	}
	return false
}

// TypeExts 返回某个类别包含的全部扩展名（用于 type: 过滤）。
func TypeExts(kind string) []string {
	var out []string
	for e, f := range formats {
		if f.kind == kind {
			out = append(out, e)
		}
	}
	return out
}
