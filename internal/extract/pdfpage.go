package extract

import (
	"unicode"
)

// PDF 每一页用文字层还是 OCR：
//   - 没有文字层：OCR；
//   - 文字层是乱码（字体没有编码对照表，取出来是私用区字符、替换符、或把 GBK 字节当成西文字母）：OCR；
//   - 文字很少、同时有图片铺满大半页：多半是扫描页上叠了页码、电子章、页眉，正文在图片里：
//     保留这些文字，同时 OCR；
//   - 其他：用文字层。
const (
	scanTextMax  = 50  // 「文字很少」：不算空白少于这么多字
	scanImageMin = 0.5 // 「图片铺满大半页」：图片面积占页面一半以上
)

type pageAction int

const (
	pageUseText     pageAction = iota // 用文字层
	pageOCR                           // 不用文字层，OCR
	pageOCRKeepText                   // 保留文字层，同时 OCR
)

// classifyPage 判断一页怎么处理。coverage 为图片覆盖页面的比例；不知道时传 0。
func classifyPage(text string, coverage float64) pageAction {
	n := 0
	for _, r := range text {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	switch {
	case n == 0 || garbledText(text):
		return pageOCR
	case n < scanTextMax && coverage >= scanImageMin:
		return pageOCRKeepText
	}
	return pageUseText
}

// garbledText 判断文字层是不是乱码。
func garbledText(s string) bool {
	total, bad, latin, han := 0, 0, 0, 0
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			continue
		case r == 0xFFFD, r >= 0xE000 && r <= 0xF8FF, r >= 0xF0000, r < 0x20, r >= 0x7F && r < 0xA0:
			bad++ // 替换符、私用区、控制字符
		case r >= 0xA0 && r <= 0xFF:
			latin++ // 西文扩展字母和符号：GBK 字节被当成西文时几乎全是这些
		case unicode.Is(unicode.Han, r):
			han++
		}
		total++
	}
	if total < 4 {
		return false
	}
	return bad*10 > total*3 || (han == 0 && latin*2 > total)
}
