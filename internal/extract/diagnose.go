package extract

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DescribeExit 把外部组件的失败翻译成能看懂的原因（尤其是 Windows 的 NTSTATUS 退出码）。
func DescribeExit(err error, output string) string {
	var ee *exec.ExitError
	msg := err.Error()
	if errors.As(err, &ee) {
		code := uint32(ee.ExitCode())
		switch code {
		case 0xC0000135:
			msg = "组件无法启动，缺少运行库 DLL（多半是 Microsoft Visual C++ 2015-2022 x64 运行库：MSVCP140.dll、VCRUNTIME140.dll）"
		case 0xC000007B:
			msg = "组件无法启动：程序或 DLL 的 32/64 位不匹配，或文件已损坏"
		case 0xC0000142:
			msg = "组件无法启动：DLL 初始化失败"
		case 0xC0000005:
			msg = "组件崩溃（访问冲突）"
		case 0xC0000409:
			msg = "组件崩溃（栈缓冲区溢出）"
		default:
			if code > 0xC0000000 {
				msg = fmt.Sprintf("组件异常退出（代码 0x%08X）", code)
			} else {
				msg = fmt.Sprintf("组件退出码 %d", ee.ExitCode())
			}
		}
	}
	if o := strings.TrimSpace(output); o != "" {
		msg += "；输出：" + truncate(o, 300)
	}
	return msg
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ComponentStatus 是一个外部组件的自检结果。
type ComponentStatus struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	OK       bool    `json:"ok"`
	Message  string  `json:"message"`
	Checking bool    `json:"checking,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
}

// CheckSoffice 用一个小 RTF 走一遍完整的老格式转换，验证 LibreOffice 真能用。
func CheckSoffice() ComponentStatus {
	st := ComponentStatus{Name: "格式转换（LibreOffice）", Path: SofficePath()}
	if st.Path == "" {
		st.Message = "未找到。老格式（doc/xls/ppt/wps 等）将无法提取内容"
		return st
	}
	dir, err := os.MkdirTemp("", "zhaozhao-check-")
	if err != nil {
		st.Message = err.Error()
		return st
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "check.rtf")
	_ = os.WriteFile(src, []byte(`{\rtf1\ansi zhaozhaocheck}`), 0o600)
	// 先试常驻方式（正常索引时用的方式），不可用再试每个文件单独启动
	start := time.Now()
	out, err := checkResident(src, dir)
	if err == nil {
		r := extractConverted(out, "docx", "")
		if r.Status == "ok" && strings.Contains(r.Text, "zhaozhaocheck") {
			st.Seconds = time.Since(start).Seconds()
			st.OK, st.Message = true, fmt.Sprintf("可用（常驻转换，启动并试转换用时 %.1f 秒）", st.Seconds)
			return st
		}
		err = errors.New("试转换结果不对：" + r.Err)
	}
	resident := err.Error()
	start = time.Now()
	r := convertLegacy(src, "rtf", 3*time.Minute, filepath.Join(dir, "profile"))
	st.Seconds = time.Since(start).Seconds()
	if r.Status == "ok" && strings.Contains(r.Text, "zhaozhaocheck") {
		st.OK, st.Message = true, fmt.Sprintf("可用（试转换用时 %.1f 秒）；常驻转换不可用，每个文件需单独启动，转换较慢：%s", st.Seconds, resident)
		return st
	}
	st.Message = "不可用：" + r.Err
	return st
}

// CheckPDFium 加载 PDFium 并读取一个小 PDF，验证它真能用。
func CheckPDFium() ComponentStatus {
	st := ComponentStatus{Name: "PDF 引擎（PDFium）"}
	p, err := PDFiumPath()
	st.Path = p
	if err != nil {
		st.Message = "不可用（" + err.Error() + "）：PDF 改用内置解析器，JBIG2、JPEG 2000 格式的扫描页无法识别"
		return st
	}
	dir, err := os.MkdirTemp("", "zhaozhao-check-")
	if err != nil {
		st.Message = err.Error()
		return st
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "check.pdf")
	_ = os.WriteFile(f, simplePDF("zhaozhaocheck"), 0o600)
	start := time.Now()
	r, ok := extractPDFium(f, DefaultOptions())
	st.Seconds = time.Since(start).Seconds()
	if !ok || r.Status != "ok" || !strings.Contains(r.Text, "zhaozhaocheck") {
		st.Message = "不可用：试读取 PDF 失败 " + r.Err
		return st
	}
	st.OK, st.Message = true, "可用"
	return st
}

// simplePDF 生成一个最简单的 PDF：每页一行英文（标准 14 字体，不需要嵌入字体）。
func simplePDF(lines ...string) []byte {
	var buf bytes.Buffer
	var offs []int
	obj := func(s string) {
		offs = append(offs, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offs), s)
	}
	buf.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	kids := ""
	for i := range lines {
		kids += fmt.Sprintf("%d 0 R ", 4+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(lines)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, l := range lines {
		content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", l)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return buf.Bytes()
}

// CheckOCR 启动 OCR 组件并识别一张空白图片，验证它真能用。
func CheckOCR(c OCRConfig) ComponentStatus {
	st := ComponentStatus{Name: "OCR（RapidOCR）", Path: c.Command}
	if !c.Enabled() {
		st.Message = "未找到。图片和扫描 PDF 只索引文件名"
		return st
	}
	dir, err := os.MkdirTemp("", "zhaozhao-check-")
	if err != nil {
		st.Message = err.Error()
		return st
	}
	defer os.RemoveAll(dir)
	img := image.NewGray(image.Rect(0, 0, 320, 240))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	img.Set(0, 0, color.Gray{0})
	p := filepath.Join(dir, "blank.png")
	f, err := os.Create(p)
	if err != nil {
		st.Message = err.Error()
		return st
	}
	_ = png.Encode(f, img)
	f.Close()
	start := time.Now()
	proc, err := startOCR(c)
	if err != nil {
		st.Message = "不可用：" + err.Error()
		return st
	}
	defer killOCR(proc)
	if _, err := recognizeWith(proc, c, p); err != nil {
		st.Message = "不可用：" + err.Error()
		return st
	}
	st.Seconds = time.Since(start).Seconds()
	st.OK, st.Message = true, fmt.Sprintf("可用（启动并识别用时 %.1f 秒）", st.Seconds)
	return st
}
