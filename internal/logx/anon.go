package logx

import (
	"crypto/sha1"
	"encoding/hex"
	"path"
	"regexp"
	"strings"
)

// P 标记日志里的文件路径。导出诊断信息时选择「隐藏文件名和路径」，被标记的路径会整段替换成代号。
func P(p string) string { return "«" + p + "»" }

// 一次扫描同时匹配两种路径，每段文字只处理一次：
//   - «…»：程序自己写日志时标记的路径；
//   - 未标记的 Windows 路径（盘符或 UNC 开头）：多出现在系统错误信息里，形如
//     「open Z:\资料\a b.pdf: 拒绝访问」，路径到冒号、换行、引号或中文标点为止。
var rePath = regexp.MustCompile(`«[^»\n]*»|(?:\b[A-Za-z]:\\|\\\\)[^\n"'«»<>|*?:，。；（）]*`)

// Anonymize 把文本里的文件路径替换成代号：每一级目录名、文件名换成由名称哈希得到的短代号，
// 保留盘符、层级和扩展名。同一个名称总是得到同一个代号，便于对照排查。
func Anonymize(s string) string {
	return rePath.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "«") {
			return "«" + anonPath(strings.TrimSuffix(strings.TrimPrefix(m, "«"), "»")) + "»"
		}
		trail := m[len(strings.TrimRight(m, " ")):] // 保留路径后面的空格
		return anonPath(strings.TrimRight(m, " ")) + trail
	})
}

func token(kind, name string) string {
	h := sha1.Sum([]byte(strings.ToLower(name)))
	return kind + hex.EncodeToString(h[:3])
}

// anonPath 逐级替换路径中的名称。
func anonPath(p string) string {
	sep := `\`
	if !strings.Contains(p, `\`) && strings.Contains(p, "/") {
		sep = "/"
	}
	lead := ""
	switch {
	case strings.HasPrefix(p, `\\`):
		lead, p = `\\`, p[2:]
	case len(p) >= 2 && p[1] == ':':
		lead, p = p[:2], p[2:]
	case strings.HasPrefix(p, "/"):
		lead, p = "/", p[1:]
	}
	parts := strings.Split(p, sep)
	for i, s := range parts {
		if s == "" || s == "." || s == ".." {
			continue
		}
		if strings.Contains(s, "›") { // 压缩包、邮件里的条目
			sub := strings.Split(s, "›")
			for k := range sub {
				sub[k] = anonName(strings.TrimSpace(sub[k]), true)
			}
			parts[i] = strings.Join(sub, " › ")
			continue
		}
		parts[i] = anonName(s, i == len(parts)-1)
	}
	return lead + strings.Join(parts, sep)
}

func anonName(s string, last bool) string {
	if s == "" {
		return s
	}
	if !last {
		return token("目录", s)
	}
	ext := path.Ext(s)
	if len(ext) > 6 || strings.ContainsAny(ext, " ") {
		ext = ""
	}
	return token("文件", s) + ext
}
