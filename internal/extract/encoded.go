package extract

import (
	"html"
	"sort"
	"strings"
)

// 文字里夹着的编码数据（base64、十六进制）：XML 里嵌的照片（如任免审批表 .lrmx 的照片）、网页里嵌的图片等。
// 它们对搜索毫无用处，却占地方：一个 5 MB 的 lrmx 提取出 526 万个字母数字，压缩后 3.7 MB，全文索引里
// 每个随机串都是一个新词。某位用户的索引库里 lrmx 占了提取文本的 55%。提取时去掉（finish 里统一处理）。

const (
	encMinRun  = 200 // 连续这么多个编码字符、中间没有空格，才算编码数据（网址、文件指纹都到不了这么长）
	encMinLine = 40  // 折行的编码数据：每行至少这么长、长度相同，连续 3 行以上
)

// isEnc：base64（含网址安全的 - _ 和补位的 =）、十六进制用到的字符。
func isEnc(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_'
}

// looksEncoded：像随机的编码数据：大写、小写字母和数字都有（base64），或者只有十六进制数字且字母数字都有。
// 只有字母（一长串单词）或只有数字（一长串数）都不算。
func looksEncoded(s string) bool {
	var up, low, dig, nonHex bool
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z':
			up, nonHex = true, nonHex || c > 'F'
		case c >= 'a' && c <= 'z':
			low, nonHex = true, nonHex || c > 'f'
		case c >= '0' && c <= '9':
			dig = true
		default:
			nonHex = true
		}
	}
	return up && low && dig || !nonHex && dig && (up || low)
}

// dropEncoded 去掉 s 里的编码数据：一行里连续 encMinRun 个以上的编码字符，以及折行的编码数据块。
// 换行都保留（按行定位的结果行号不变）；keepLen 为 true 时把编码字符换成空格，按字符位置定位的结果位置也不变。
func dropEncoded(s string, keepLen bool) string {
	var cut [][2]int
	// 一行里的长串
	for i := 0; i < len(s); {
		if !isEnc(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isEnc(s[j]) {
			j++
		}
		if j-i >= encMinRun && looksEncoded(s[i:j]) {
			cut = append(cut, [2]int{i, j})
		}
		i = j
	}
	// 折行的编码数据：连续几行都只有编码字符、长度相同（最后一行可以短一些）
	type line struct{ a, b int }
	var group []line
	flush := func() {
		if len(group) >= 3 {
			total := 0
			for _, l := range group {
				total += l.b - l.a
			}
			if total >= encMinRun && looksEncoded(s[group[0].a:group[0].b]+s[group[1].a:group[1].b]) {
				for _, l := range group {
					cut = append(cut, [2]int{l.a, l.b})
				}
			}
		}
		group = group[:0]
	}
	for start := 0; start < len(s); {
		end := strings.IndexByte(s[start:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += start
		}
		a, b := start, end
		for a < b && (s[a] == ' ' || s[a] == '\t') {
			a++
		}
		for b > a && (s[b-1] == ' ' || s[b-1] == '\t' || s[b-1] == '\r') {
			b--
		}
		pure := b-a >= encMinLine
		for k := a; pure && k < b; k++ {
			pure = isEnc(s[k])
		}
		switch {
		case !pure:
			flush()
		case len(group) > 0 && (b-a > group[0].b-group[0].a || group[len(group)-1].b-group[len(group)-1].a < group[0].b-group[0].a):
			// 比第一行长，或者前一行已经是短的最后一行：另起一组
			flush()
			group = append(group, line{a, b})
		default:
			group = append(group, line{a, b})
		}
		start = end + 1
	}
	flush()
	if len(cut) == 0 {
		return s
	}
	sort.Slice(cut, func(i, j int) bool { return cut[i][0] < cut[j][0] })
	var out strings.Builder
	out.Grow(len(s) / 2)
	last := 0
	for _, c := range cut {
		if c[0] < last {
			c[0] = last
		}
		if c[0] >= c[1] {
			continue
		}
		out.WriteString(s[last:c[0]])
		if keepLen {
			out.WriteString(strings.Repeat(" ", c[1]-c[0]))
		}
		last = c[1]
	}
	out.WriteString(s[last:])
	return out.String()
}

// xmlText 取出 XML 里的文字：元素里的文字、属性值（命名空间声明除外）和 CDATA，每段一行；
// 去掉标签、注释、声明。容错：标签不完整也照样取。用于任免审批表（.lrmx）这类 XML 文档。
func xmlText(raw []byte) string {
	src := plainText(DecodeText(raw))
	var b strings.Builder
	add := func(t string, unescape bool) {
		if unescape {
			t = html.UnescapeString(t)
		}
		if t = strings.TrimSpace(dropEncoded(t, false)); t != "" { // 整段都是编码数据（照片）的不留空行
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			add(src[i:], true)
			break
		}
		add(src[i:i+lt], true)
		i += lt
		rest := src[i:]
		switch {
		case strings.HasPrefix(rest, "<![CDATA["):
			end := strings.Index(rest, "]]>")
			if end < 0 {
				add(rest[9:], false)
				i = len(src)
			} else {
				add(rest[9:end], false)
				i += end + 3
			}
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest, "-->")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 3
			}
		default:
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				end = len(rest) - 1
			}
			if len(rest) > 1 && rest[1] != '?' && rest[1] != '!' && rest[1] != '/' {
				for _, v := range xmlAttrValues(rest[1:end]) {
					add(v, true)
				}
			}
			i += end + 1
		}
	}
	return b.String()
}

// xmlAttrValues 返回标签里各属性的值（跳过 xmlns 命名空间声明）。
func xmlAttrValues(tag string) []string {
	var out []string
	for {
		eq := strings.IndexByte(tag, '=')
		if eq < 0 {
			return out
		}
		name := strings.TrimSpace(tag[:eq])
		if k := strings.LastIndexAny(name, " \t\n"); k >= 0 {
			name = name[k+1:]
		}
		rest := strings.TrimLeft(tag[eq+1:], " \t\n")
		if rest == "" || (rest[0] != '"' && rest[0] != '\'') {
			return out
		}
		end := strings.IndexByte(rest[1:], rest[0])
		if end < 0 {
			return out
		}
		if name != "xmlns" && !strings.HasPrefix(name, "xmlns:") {
			out = append(out, rest[1:1+end])
		}
		tag = rest[end+2:]
	}
}
