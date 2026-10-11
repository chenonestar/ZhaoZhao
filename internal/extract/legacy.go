package extract

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// 老格式经 LibreOffice 无头模式转换为 OOXML 后再提取。
var legacyTarget = map[string]string{
	"doc": "docx", "dot": "docx", "wps": "docx", "wpt": "docx", "rtf": "docx",
	"xls": "xlsx", "xlt": "xlsx", "et": "xlsx", "ett": "xlsx",
	"ppt": "pptx", "pps": "pptx", "pot": "pptx", "dps": "pptx", "dpt": "pptx",
}

var (
	sofficeOnce sync.Once
	sofficePath string
	profileDir  string
)

// SofficePath 返回可用的 LibreOffice 可执行文件路径，找不到返回空串。
func SofficePath() string {
	sofficeOnce.Do(func() {
		var cands []string
		if p := os.Getenv("ZHAOZHAO_SOFFICE"); p != "" {
			cands = append(cands, p)
		}
		if exe, err := os.Executable(); err == nil {
			dir := filepath.Dir(exe)
			cands = append(cands,
				filepath.Join(dir, "components", "libreoffice", "program", "soffice.exe"),
				filepath.Join(dir, "components", "libreoffice", "program", "soffice"))
		}
		if runtime.GOOS == "windows" {
			for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
				if base != "" {
					cands = append(cands, filepath.Join(base, "LibreOffice", "program", "soffice.exe"))
				}
			}
		}
		for _, c := range cands {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				sofficePath = c
				return
			}
		}
		for _, n := range []string{"soffice", "libreoffice"} {
			if p, err := exec.LookPath(n); err == nil {
				sofficePath = p
				return
			}
		}
	})
	return sofficePath
}

func extractLegacy(p, ext string, timeout time.Duration) doc.Result {
	target := legacyTarget[ext]
	so := SofficePath()
	if so == "" {
		return doc.Result{Status: doc.StatusFailed, Err: "未找到格式转换组件（LibreOffice），安装后可在失败列表中重试"}
	}
	tmp, err := os.MkdirTemp("", "zhaozhao-conv-")
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	defer os.RemoveAll(tmp)
	if profileDir == "" {
		profileDir = filepath.Join(os.TempDir(), fmt.Sprintf("zhaozhao-lo-%d", os.Getpid()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// 复制到临时目录并用 ASCII 文件名，规避 LibreOffice 对特殊路径的处理问题
	src := filepath.Join(tmp, "in."+ext)
	if err := copyFile(p, src); err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: "读取文件失败: " + err.Error()}
	}
	outDir := filepath.Join(tmp, "out")
	_ = os.MkdirAll(outDir, 0o700)
	cmd := exec.CommandContext(ctx, so, "--headless", "--norestore", "--nologo", "--nodefault", "--nolockcheck",
		"-env:UserInstallation="+fileURL(profileDir), "--convert-to", target, "--outdir", outDir, src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	setProcAttr(cmd)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return doc.Result{Status: doc.StatusFailed, Err: "格式转换超时"}
		}
		return doc.Result{Status: doc.StatusFailed, Err: "格式转换失败: " + strings.TrimSpace(stderr.String())}
	}
	out := filepath.Join(outDir, "in."+target)
	if _, err := os.Stat(out); err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: "格式转换失败（文件可能已加密或损坏）"}
	}
	var r doc.Result
	switch target {
	case "docx":
		r = extractDocx(out)
	case "xlsx":
		r = extractXlsx(out)
	default:
		r = extractPptx(out)
	}
	if r.Status == doc.StatusOK && strings.TrimSpace(r.Text) == "" {
		r.Status = doc.StatusNoContent
		r.Err = "文件没有文字内容"
	}
	return r
}

// fileURL 把本地路径转成 file:// URL，非 ASCII 字符和空格做百分号编码
// （用户名含中文时，临时目录路径会带中文）。
func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
