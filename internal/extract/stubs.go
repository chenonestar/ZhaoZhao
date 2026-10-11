package extract

import "github.com/chenonestar/zhaozhao/internal/doc"

// OCRConfig 配置 OCR 组件。
type OCRConfig struct {
	Command string // 外部 OCR 命令（预留）
}

func (c OCRConfig) Enabled() bool { return c.Command != "" }

func ocrPDF(p string, opt Options) doc.Result {
	return doc.Result{Status: doc.StatusNoContent, Err: "没有文字层，需要 OCR"}
}

func extractImage(p string, opt Options) doc.Result {
	return doc.Result{Status: doc.StatusSkipped, Err: "OCR 组件未启用"}
}

func extractContainer(p, ext string, opt Options, depth int) doc.Result {
	return doc.Result{Status: doc.StatusSkipped, Err: "暂不支持"}
}
