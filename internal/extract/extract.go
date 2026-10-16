package extract

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// Options 控制提取行为。
type Options struct {
	MaxTextBytes    int64         // 文本类文件读取上限
	SniffUnknown    bool          // 未知扩展名抽样判断是否为文本
	Timeout         time.Duration // 老格式转换等外部步骤的超时
	MaxRunes        int           // 提取文本最大字符数，0 为不限
	MaxDepth        int           // 容器嵌套层数
	ArchiveMaxBytes int64         // 单个压缩包解压总量上限
	ArchiveMaxRatio int           // 单个内部文件压缩比上限
	OCR             OCRConfig
	Debug           bool   // 记录详细日志（解析子进程据此设置自己的日志级别）
	LegacyMode      string `json:"legacyMode,omitempty"` // 老格式的处理方式：LegacyAuto、LegacyNative、LegacyLO

	progress func(string) // 进度回调（不序列化：子进程里由 Serve 设置）
}

// SetProgress 设置进度回调。长时间的步骤（OCR 逐页、压缩包逐项）会报告进度，
// 主程序据此显示「第几页」，并在有进展时延长超时。
func (o *Options) SetProgress(f func(string)) { o.progress = f }

// Progress 报告进度。
func (o Options) Progress(format string, a ...any) {
	if o.progress != nil {
		o.progress(fmt.Sprintf(format, a...))
	}
}

// DefaultOptions 返回默认选项。
func DefaultOptions() Options {
	return Options{MaxTextBytes: 100 << 20, SniffUnknown: true, Timeout: 60 * time.Second, MaxRunes: 64 << 20, MaxDepth: 3, ArchiveMaxBytes: 2 << 30, ArchiveMaxRatio: 100}
}

func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	m, err := io.ReadFull(f, b)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return b[:m], err
}

// Extract 提取一个文件的文本。它可能很慢、可能崩溃，应在子进程中调用。
func Extract(p, ext string, opt Options) (res doc.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = doc.Result{Status: doc.StatusFailed, Err: fmt.Sprintf("解析异常: %v", r)}
		}
		if res.Status == doc.StatusOK {
			finish(&res, opt)
		}
	}()
	f, known := formats[ext]
	if !known || f.h == hNone {
		if !known && opt.SniffUnknown {
			head, err := readHead(p, 8192)
			if err != nil {
				return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
			}
			if LooksLikeText(head) {
				return extractText(p, opt)
			}
		}
		return doc.Result{Status: doc.StatusSkipped, Err: "不支持的文件类型"}
	}
	if (f.h == hDocx || f.h == hXlsx || f.h == hPptx) && isOLE(p) {
		// OLE 格式的 docx/xlsx/pptx 通常是加密的；也可能是改了扩展名的 doc/xls/ppt
		fam := map[handler]string{hDocx: "doc", hXlsx: "xls", hPptx: "ppt"}[f.h]
		names, _ := oleRootNames(p)
		if !names["WordDocument"] && !names["Workbook"] && !names["Book"] && !names["PowerPoint Document"] {
			return extractEncryptedOOXML(p, fam)
		}
		return extractLegacyAny(p, fam, opt)
	}
	switch f.h {
	case hText:
		return extractText(p, opt)
	case hHTML:
		raw, err := readLimited(p, opt.MaxTextBytes)
		if err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
		}
		return doc.Result{Status: doc.StatusOK, Text: htmlText(raw), LineBased: true}
	case hDocx:
		return extractDocx(p)
	case hXlsx:
		return extractXlsx(p)
	case hPptx:
		return extractPptx(p)
	case hODF:
		return extractODF(p)
	case hPDF:
		return extractPDF(p, opt)
	case hLegacy:
		return extractLegacyAny(p, ext, opt)
	case hImage:
		return extractImage(p, ext, opt)
	case hZip, hEML, hMbox, hMSG:
		return extractContainer(p, ext, opt, 0)
	}
	return doc.Result{Status: doc.StatusSkipped, Err: "不支持的文件类型"}
}

func readLimited(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if max <= 0 {
		max = 100 << 20
	}
	return io.ReadAll(io.LimitReader(f, max))
}

func extractText(p string, opt Options) doc.Result {
	raw, err := readLimited(p, opt.MaxTextBytes)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	return doc.Result{Status: doc.StatusOK, Text: plainText(DecodeText(raw)), LineBased: true}
}

// finish 统一收尾：去掉非法字符、限制长度。
func finish(r *doc.Result, opt Options) {
	r.Text = strings.ToValidUTF8(r.Text, "")
	if opt.MaxRunes > 0 && len(r.Text) > opt.MaxRunes {
		rs := []rune(r.Text)
		if len(rs) > opt.MaxRunes {
			r.Text = string(rs[:opt.MaxRunes])
			for len(r.Locs) > 0 && r.Locs[len(r.Locs)-1].Off >= opt.MaxRunes {
				r.Locs = r.Locs[:len(r.Locs)-1]
			}
		}
	}
	if strings.TrimSpace(r.Text) == "" && len(r.Children) == 0 {
		r.Status = doc.StatusNoContent
		r.Err = "没有文字内容"
	}
}
