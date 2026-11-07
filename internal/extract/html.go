package extract

import (
	"strings"

	"golang.org/x/net/html"
)

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "table": true, "section": true, "article": true, "pre": true,
	"ul": true, "ol": true, "blockquote": true, "hr": true, "title": true, "dt": true, "dd": true,
}

// htmlText 去除标签后取正文；先尝试按 meta/BOM/内容自动识别编码。
func htmlText(raw []byte) string {
	src := DecodeText(raw)
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return plainText(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			n := string(name)
			if n == "script" || n == "style" {
				skip++
			} else if blockTags[n] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := string(name)
			if n == "script" || n == "style" {
				if skip > 0 {
					skip--
				}
			} else if blockTags[n] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if skip == 0 {
				t := z.Text()
				b.WriteString(collapseSpace(string(t)))
			}
		}
	}
}

func collapseSpace(s string) string {
	var b strings.Builder
	sp := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !sp {
				b.WriteByte(' ')
			}
			sp = true
			continue
		}
		sp = false
		b.WriteRune(r)
	}
	return b.String()
}
