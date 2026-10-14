package extract

import (
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
