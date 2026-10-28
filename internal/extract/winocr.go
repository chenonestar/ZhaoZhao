package extract

import (
	"image"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// joinWords 把 Windows OCR 认出的一行「词」拼起来：英文、数字的词之间是空格；中文每个字（或词）单独一个，
// 拼回去时不加空格。
func joinWords(ws []string) string {
	var b strings.Builder
	for i, w := range ws {
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(ws[i-1])
			next, _ := utf8.DecodeRuneInString(w)
			if !wideRune(prev) && !wideRune(next) {
				b.WriteByte(' ')
			}
		}
		b.WriteString(w)
	}
	return b.String()
}

// wideRune 说明是不是中日韩文字或全角符号（这些字前后不需要空格）。
func wideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef)
}

// WinOCRImage 用 Windows 自带的 OCR 认一张图片（打包测试的 packaging/tools/winocr 用）。
func WinOCRImage(img image.Image) (string, error) { return winOCRImage(img, 2*time.Minute) }
