package extract

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

const commonHan = "的一是不了人我在有他这中大来上国个到说们为子和你地出道也时年得就那要下以生会自着去之过家学对可她里后小么心多天而能好都然没日于起还发成事只作当想看文无开手十用主行方又如前所本见经头面公同三已老从动两长知民样现分将外但身些与高意进把法此实回二理美点月明其种声全工话儿者向情部正名定女问力机给等几很业最间新什少它重吗做"
const commonHant = "的一是不了人我在有他這中大來上國個到說們為子和你地出道也時年得就那要下以生會自著去之過家學對可她裡後小麼心多天而能好都然沒日於起還發成事只作當想看文無開手十用主行方又如前所本見經頭面公同三已老從動兩長知民樣現分將外但身些與高意進把法此實回二理美點月明其種聲全工話兒者向情部正名定女問力機給等幾很業最間新什少它重嗎做"

var (
	hanSet  = runeSet(commonHan)
	hantSet = runeSet(commonHant)
)

func runeSet(s string) map[rune]bool {
	m := map[rune]bool{}
	for _, r := range s {
		m[r] = true
	}
	return m
}

// DecodeText 自动识别编码并转成 UTF-8：UTF-8（含 BOM）、UTF-16、GBK/GB18030、Big5，
// 无法确定时按 GB18030 处理。
func DecodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:])
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], false)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], true)
	}
	if looksUTF16(b) {
		return decodeUTF16(b, evenNULs(b) > oddNULs(b))
	}
	if utf8.Valid(b) {
		return string(b)
	}
	gb, gbErr := simplifiedchinese.GB18030.NewDecoder().Bytes(b)
	big, bigErr := traditionalchinese.Big5.NewDecoder().Bytes(b)
	switch {
	case gbErr == nil && bigErr != nil:
		return string(gb)
	case gbErr != nil && bigErr == nil:
		return string(big)
	case gbErr == nil && bigErr == nil:
		if score(string(big), hantSet) > score(string(gb), hanSet) {
			return string(big)
		}
		return string(gb)
	}
	// 都有非法字节：按 GB18030 宽松解码
	out, _ := simplifiedchinese.GB18030.NewDecoder().String(strings.ToValidUTF8(string(b), "?"))
	return out
}

func score(s string, set map[rune]bool) int {
	n := 0
	for _, r := range s {
		if set[r] {
			n++
		}
	}
	return n
}

func evenNULs(b []byte) int { return countNUL(b, 0) }
func oddNULs(b []byte) int  { return countNUL(b, 1) }

func countNUL(b []byte, start int) int {
	n := 0
	for i := start; i < len(b) && i < 4096; i += 2 {
		if b[i] == 0 {
			n++
		}
	}
	return n
}

// looksUTF16 判断无 BOM 的数据是否像 UTF-16：大量字节为 0 且集中在奇数或偶数位。
func looksUTF16(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	n := len(b)
	if n > 4096 {
		n = 4096
	}
	e, o := evenNULs(b[:n]), oddNULs(b[:n])
	half := n / 2
	return (e > half*3/10 && o == 0) || (o > half*3/10 && e == 0)
}

func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u))
}

// LooksLikeText 抽样判断未知扩展名的文件是不是文本。
func LooksLikeText(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	if bytes.HasPrefix(sample, []byte{0xEF, 0xBB, 0xBF}) || bytes.HasPrefix(sample, []byte{0xFF, 0xFE}) || bytes.HasPrefix(sample, []byte{0xFE, 0xFF}) {
		return true
	}
	if looksUTF16(sample) {
		return true
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return false
	}
	ctrl := 0
	for _, c := range sample {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' && c != '\f' && c != 0x1b {
			ctrl++
		}
	}
	return ctrl*100 <= len(sample)
}

// plainText 把解码后的文本整理成行文本，并统一换行符。
func plainText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\x00", "")
}
