package extract

import (
	"image"
	"image/draw"
	"sort"
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

// WinOCRImage 用 Windows 自带的 OCR 认一张图片，方向不对时转过来再认（打包测试的 packaging/tools/winocr 用）。
func WinOCRImage(img image.Image) (string, error) {
	text, _, err := winOCRBest(img, func(i image.Image) (string, error) { return winOCRImage(i, 2*time.Minute) })
	return text, err
}

// winLine 是 Windows OCR 认出的一行和它在图上的位置（像素）。
type winLine struct {
	text           string
	x0, y0, x1, y1 float64
}

// orderLines 把认出的行按位置排好拼起来：从上到下，同一排的（竖直方向大半重叠）从左到右，和 PP-OCRv6（RapidOCR）
// 的顺序一致。Windows OCR 自己的顺序是按它分出的文字块排的，表格、表单常常一列一列地读：0.2.8 某位用户的 20 个
// 扫描件里，认出的字和 PP-OCRv6 差不多，按顺序比却只有一半相同，跨格子的短语也搜不到。
func orderLines(ls []winLine) string {
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].y0+ls[i].y1 < ls[j].y0+ls[j].y1 })
	var rows [][]winLine
	for _, l := range ls {
		if n := len(rows); n > 0 && sameRow(rows[n-1][0], l) {
			rows[n-1] = append(rows[n-1], l)
			continue
		}
		rows = append(rows, []winLine{l})
	}
	var b strings.Builder
	for _, row := range rows {
		sort.SliceStable(row, func(i, j int) bool { return row[i].x0 < row[j].x0 })
		for _, l := range row {
			b.WriteString(l.text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// sameRow：b 的竖直中线落在 a 的上下边之间、且两行高度相差不到一倍，算同一排。
func sameRow(a, b winLine) bool {
	ha, hb := a.y1-a.y0, b.y1-b.y0
	c := (b.y0 + b.y1) / 2
	return c > a.y0 && c < a.y1 && hb < 2*ha && ha < 2*hb
}

// ocrScore 给认出的文字打分：汉字算 1 分，字母、数字算 0.3 分（转错方向时常认出一些零散的字母、符号）。
func ocrScore(s string) float64 {
	var v float64
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			v++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			v += 0.3
		}
	}
	return v
}

// winOCRWeak：认出的字太少，或者大半不是汉字（多半方向不对，认出的是乱码），值得转个方向再认。
func winOCRWeak(s string) bool {
	n, han := 0, 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		n++
		if unicode.Is(unicode.Han, r) {
			han++
		}
	}
	return n < 50 || han*2 < n
}

// winOCRBest 用 Windows OCR 认一页，方向不对时转过来再认：横向的页（多半是竖版文件转了 90° 扫描的）再试转 90°、
// 270°，认出的字太少或像乱码的页再试其余方向，取得分最高的。Windows OCR 只认正着的字；PP-OCRv6 会把竖着的
// 文字框转过来认。0.2.8 某位用户两万多页扫描件里，Windows OCR 比 PP-OCRv6 少认的字有 58% 在 1551 个横向页上。
// 返回认出的文字和转了多少度（0、90、180、270，顺时针）。
func winOCRBest(img image.Image, recognize func(image.Image) (string, error)) (string, int, error) {
	g := toGray(img)
	text, err := recognize(g)
	if err != nil {
		return "", 0, err
	}
	b := g.Bounds()
	landscape := b.Dx() > b.Dy()
	if !landscape && !winOCRWeak(text) {
		return text, 0, nil
	}
	best, bestDeg, bestScore := text, 0, ocrScore(text)
	try := func(deg int) error {
		t, err := recognize(rotateGray(g, deg))
		if err != nil {
			return err
		}
		// 要明显更好才换（同一页转错方向偶尔也认出几个字）
		if s := ocrScore(t); s > bestScore*1.2+5 {
			best, bestDeg, bestScore = t, deg, s
		}
		return nil
	}
	order := []int{180, 90, 270}
	if landscape {
		order = []int{90, 270, 180}
	}
	for i, deg := range order {
		if landscape && i == 2 && !winOCRWeak(best) {
			break // 横向页转 90°/270° 已经认好了，不用再试 180°
		}
		if err := try(deg); err != nil {
			return "", 0, err
		}
	}
	return best, bestDeg, nil
}

// toGray 转成 8 位灰度图（OCR 不需要颜色；透明的地方当白底）。
func toGray(img image.Image) *image.Gray {
	if g, ok := img.(*image.Gray); ok {
		return g
	}
	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(g, g.Rect, image.White, image.Point{}, draw.Src)
	draw.Draw(g, g.Rect, img, b.Min, draw.Over)
	return g
}

// rotateGray 把灰度图顺时针转 deg 度（90 的倍数）。
func rotateGray(g *image.Gray, deg int) *image.Gray {
	b := g.Bounds()
	w, h := b.Dx(), b.Dy()
	var out *image.Gray
	switch deg % 360 {
	case 90, 270:
		out = image.NewGray(image.Rect(0, 0, h, w))
	case 180:
		out = image.NewGray(image.Rect(0, 0, w, h))
	default:
		return g
	}
	for y := 0; y < h; y++ {
		row := g.Pix[(y)*g.Stride : y*g.Stride+w]
		for x, v := range row {
			switch deg % 360 {
			case 90: // (x, y) → (h-1-y, x)
				out.Pix[x*out.Stride+(h-1-y)] = v
			case 270: // (x, y) → (y, w-1-x)
				out.Pix[(w-1-x)*out.Stride+y] = v
			case 180:
				out.Pix[(h-1-y)*out.Stride+(w-1-x)] = v
			}
		}
	}
	return out
}
