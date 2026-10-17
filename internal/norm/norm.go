// Package norm 负责搜索前的文本归一化与分词。
//
// 归一化保证「一个字符变一个字符」，因此归一化后的文本与原文的字符偏移一一对应，
// 命中位置可以直接用于原文高亮。
package norm

import (
	"strings"
	"unicode"
)

// Options 控制可选的归一化项。大小写与全半角始终生效。
type Options struct {
	// Simplify 为 true 时把常见繁体字折叠为简体字。
	Simplify bool
}

var punct = map[rune]rune{
	'，': ',', '。': '.', '、': ',', '；': ';', '：': ':', '？': '?', '！': '!',
	'“': '"', '”': '"', '‘': '\'', '’': '\'', '（': '(', '）': ')', '【': '[', '】': ']',
	'《': '<', '》': '>', '—': '-', '～': '~', '·': '.',
}

// Rune 归一化单个字符。
func (o Options) Rune(r rune) rune {
	if r < 0x80 {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	switch {
	case r == 0x3000:
		return ' '
	case r >= 0xFF01 && r <= 0xFF5E:
		r = r - 0xFF01 + 0x21
		if r >= 'A' && r <= 'Z' {
			r += 32
		}
		return r
	}
	if r >= 0x4E00 && r <= 0x9FFF { // 常用汉字：不是标点、没有大小写，只看繁简
		if o.Simplify {
			if s, ok := t2s[r]; ok {
				return s
			}
		}
		return r
	}
	if p, ok := punct[r]; ok {
		return p
	}
	if o.Simplify {
		if s, ok := t2s[r]; ok {
			return s
		}
	}
	if unicode.IsUpper(r) {
		if l := unicode.ToLower(r); l != 0 {
			return l
		}
	}
	return r
}

// Runes 归一化整段文本，返回与输入等长（按字符计）的结果。
func (o Options) Runes(s string) []rune {
	out := make([]rune, 0, len(s)/2+1)
	for _, r := range s {
		out = append(out, o.Rune(r))
	}
	return out
}

// String 归一化字符串。
func (o Options) String(s string) string { return string(o.Runes(s)) }

// IsCJK 判断字符是否按「单字成词」处理（汉字、假名、谚文）。
func IsCJK(r rune) bool {
	if r < 0x1100 {
		return false
	}
	if r >= 0x4E00 && r <= 0x9FFF { // 常用汉字（最常见的情况，不必查表）
		return true
	}
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// Token 是一个词元，Start/End 为字符偏移（左闭右开）。
type Token struct {
	Text       string
	Start, End int
	CJK        bool
}

// Tokenize 对已归一化的字符序列分词：每个 CJK 字符一个词元，
// 连续的字母数字为一个词元，其余视为分隔符。
func Tokenize(rs []rune) []Token {
	var toks []Token
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case IsCJK(r):
			toks = append(toks, Token{Text: string(r), Start: i, End: i + 1, CJK: true})
			i++
		case isWord(r):
			j := i
			for j < len(rs) && isWord(rs[j]) && !IsCJK(rs[j]) {
				j++
			}
			toks = append(toks, Token{Text: string(rs[i:j]), Start: i, End: j})
			i = j
		default:
			i++
		}
	}
	return toks
}

// FTSText 把词元用空格连接，作为 FTS5（unicode61 分词器）的入库文本。
func FTSText(toks []Token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t.Text)
	}
	return b.String()
}

// FTS 归一化并分词，直接生成 FTS5 的入库文本：与 FTSText(Tokenize(o.Runes(s))) 结果相同，
// 但一次遍历写完，不产生中间的字符切片和词元切片（几 MB 的中文文本能省下几百 MB 临时内存）。
func (o Options) FTS(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/3) // 汉字之间要插空格：每 3 字节多 1 字节
	inWord, empty := false, true
	for _, r := range s {
		r = o.Rune(r)
		switch {
		case IsCJK(r):
			if !empty {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			inWord, empty = false, false
		case isWord(r):
			if !inWord {
				if !empty {
					b.WriteByte(' ')
				}
				inWord, empty = true, false
			}
			b.WriteRune(r)
		default:
			inWord = false
		}
	}
	return b.String()
}

// Adjacent 判断两个相邻词元在原文中是否紧挨着（中间没有任何字符）。
func Adjacent(a, b Token) bool { return a.End == b.Start }
