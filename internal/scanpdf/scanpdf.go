// Package scanpdf 生成「扫描版」PDF（每页一张图片、没有文字层），用于测试 OCR 流程。
package scanpdf

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"io"
)

// Page 是一页：JPEG 图片，或 JBIG2 为 true 时的一个（无法识别的）JBIG2 图片对象。
// Text 非空时在图片上叠一行文字（英文，标准字体），模拟扫描页上加的页码、电子章。
// PUAText 非空时叠一行「乱码」文字：字体编码把字符映射到私用区，文字层取出来是乱码。
type Page struct {
	Image   image.Image
	JBIG2   bool
	Text    string
	PUAText string
}

// Write 写出 PDF。
func Write(w io.Writer, pages []Page) error {
	var buf bytes.Buffer
	var offs []int
	obj := func(body string, stream []byte) int {
		offs = append(offs, buf.Len())
		n := len(offs)
		fmt.Fprintf(&buf, "%d 0 obj\n%s", n, body)
		if stream != nil {
			buf.WriteString("\nstream\n")
			buf.Write(stream)
			buf.WriteString("\nendstream")
		}
		buf.WriteString("\nendobj\n")
		return n
	}
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>", nil)
	obj("", nil) // 页面树，最后补写
	var kids []int
	for _, p := range pages {
		w, h := 200, 200
		var data []byte
		filter := "/DCTDecode"
		cs := "/DeviceRGB"
		bpc := 8
		if p.JBIG2 {
			data = []byte("\x00\x00\x00\x00not-a-real-jbig2-stream")
			filter, cs, bpc = "/JBIG2Decode", "/DeviceGray", 1
		} else {
			b := p.Image.Bounds()
			w, h = b.Dx(), b.Dy()
			if _, gray := p.Image.(*image.Gray); gray {
				cs = "/DeviceGray" // 灰度图编码出的 JPEG 只有一个分量
			}
			var jb bytes.Buffer
			if err := jpeg.Encode(&jb, p.Image, &jpeg.Options{Quality: 90}); err != nil {
				return err
			}
			data = jb.Bytes()
		}
		img := obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent %d /Filter %s /Length %d >>",
			w, h, cs, bpc, filter, len(data)), data)
		// 页面按图片像素数的 72dpi 尺寸
		content := []byte(fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q", w, h))
		fonts := ""
		if p.Text != "" {
			f := obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", nil)
			fonts += fmt.Sprintf(" /F1 %d 0 R", f)
			content = append(content, fmt.Sprintf(" BT /F1 10 Tf 10 10 Td (%s) Tj ET", p.Text)...)
		}
		if p.PUAText != "" {
			// 把 A～Z 映射到私用区字形名（uniE041…），没有 ToUnicode：取出的文字是私用区字符
			diff := ""
			for c := 'A'; c <= 'Z'; c++ {
				diff += fmt.Sprintf(" /uniE0%02X", int(c))
			}
			f := obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding << /Type /Encoding /Differences [65"+diff+"] >> >>", nil)
			fonts += fmt.Sprintf(" /F2 %d 0 R", f)
			content = append(content, fmt.Sprintf(" BT /F2 12 Tf 10 30 Td (%s) Tj ET", p.PUAText)...)
		}
		res := fmt.Sprintf("/XObject << /Im0 %d 0 R >>", img)
		if fonts != "" {
			res += " /Font <<" + fonts + " >>"
		}
		c := obj(fmt.Sprintf("<< /Length %d >>", len(content)), content)
		kids = append(kids, obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Resources << %s >> /Contents %d 0 R >>",
			w, h, res, c), nil))
	}
	// 补写页面树（对象 2）：放在文件末尾，交叉引用表指向新位置
	kidRefs := ""
	for _, k := range kids {
		kidRefs += fmt.Sprintf("%d 0 R ", k)
	}
	offs[1] = buf.Len()
	fmt.Fprintf(&buf, "2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", kidRefs, len(kids))
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	_, err := w.Write(buf.Bytes())
	return err
}
