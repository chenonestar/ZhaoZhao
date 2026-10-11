package extract

import "github.com/chenonestar/zhaozhao/internal/doc"

func extractContainer(p, ext string, opt Options, depth int) doc.Result {
	return doc.Result{Status: doc.StatusSkipped, Err: "暂不支持"}
}

// ExtractInner 把容器内的条目解出到 destDir，返回解出文件的路径。
func ExtractInner(container, inner, destDir string, opt Options) (string, error) {
	return "", errUnsupportedInner
}

var errUnsupportedInner = errString("暂不支持打开压缩包或邮件内的文件")

type errString string

func (e errString) Error() string { return string(e) }
