package extract

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/logx"
)

// 常驻 LibreOffice：每个老格式转换子进程启动一个 soffice（--accept 管道模式）并一直保留，
// 通过 Python-UNO 桥接脚本逐个转换文件。省去了每个文件都要重新启动 LibreOffice 的 1～3 秒
// （慢电脑上更久）。没有 Python-UNO 组件（例如 Linux 上没装 python3-uno）时退回每个文件单独启动。

//go:embed lobridge.py
var loBridgeScript []byte

// loFilters 是转换目标格式对应的 LibreOffice 导出过滤器。
var loFilters = map[string]string{
	"docx": "MS Word 2007 XML",
	"xlsx": "Calc MS Excel 2007 XML",
	"pptx": "Impress MS PowerPoint 2007 XML",
}

// loRecycleAfter：转换这么多个文件后重启一次 LibreOffice，避免内存越用越多。
const loRecycleAfter = 300

type loResident struct {
	so, py   *exec.Cmd
	cancelSo context.CancelFunc
	cancelPy context.CancelFunc
	in       io.WriteCloser
	out      *bufio.Reader
	tail     *tailBuffer
	n        int
}

var (
	loMu       sync.Mutex
	loCur      *loResident
	loDisabled string // 常驻方式在本进程内不可用的原因
	loSeq      int
	loScript   string
)

// loPython 返回运行桥接脚本的 Python：优先用 LibreOffice 自带的（Windows 版都带），
// 其次是系统的 python3（Linux 发行版的 LibreOffice 需要安装 python3-uno）。
func loPython(so string) string {
	if p := os.Getenv("ZHAOZHAO_LO_PYTHON"); p != "" {
		return p
	}
	if real, err := filepath.EvalSymlinks(so); err == nil {
		so = real
	}
	dir := filepath.Dir(so)
	names := []string{"python"}
	if runtime.GOOS == "windows" {
		names = []string{"python.exe"}
	}
	for _, n := range names {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && !st.IsDir() {
			return filepath.Join(dir, n)
		}
	}
	if runtime.GOOS != "windows" {
		// 发行版自带的 LibreOffice 配套的是系统 Python（python3-uno 装在这里），优先于 PATH 里的其他 Python
		if st, err := os.Stat("/usr/bin/python3"); err == nil && !st.IsDir() {
			return "/usr/bin/python3"
		}
		if p, err := exec.LookPath("python3"); err == nil {
			return p
		}
	}
	return ""
}

// startLO 启动常驻的 soffice 和桥接脚本，等待连接成功。
func startLO(profile string, limit time.Duration) (*loResident, error) {
	so := SofficePath()
	if so == "" {
		return nil, errors.New("未找到 LibreOffice")
	}
	py := loPython(so)
	if py == "" {
		return nil, errors.New("未找到 LibreOffice 自带的 Python")
	}
	if loScript == "" {
		f := filepath.Join(os.TempDir(), fmt.Sprintf("zhaozhao-lobridge-%d.py", os.Getpid()))
		if err := os.WriteFile(f, loBridgeScript, 0o600); err != nil {
			return nil, err
		}
		loScript = f
	}
	loSeq++
	pipe := fmt.Sprintf("zhaozhao_%d_%d", os.Getpid(), loSeq)
	r := &loResident{tail: &tailBuffer{max: 4096}}
	ctxSo, cancelSo := context.WithCancel(context.Background())
	r.so = exec.CommandContext(ctxSo, so, "--headless", "--invisible", "--nocrashreport", "--nodefault", "--nofirststartwizard",
		"--nologo", "--norestore", "--nolockcheck", "-env:UserInstallation="+fileURL(profile),
		"--accept=pipe,name="+pipe+";urp;StarOffice.ComponentContext")
	r.so.Stdout, r.so.Stderr = r.tail, r.tail
	setProcAttr(r.so)
	r.cancelSo = cancelSo
	t0 := time.Now()
	if err := r.so.Start(); err != nil {
		cancelSo()
		return nil, fmt.Errorf("启动 LibreOffice 失败: %v", err)
	}
	ctxPy, cancelPy := context.WithCancel(context.Background())
	r.py = exec.CommandContext(ctxPy, py, loScript, pipe, strconv.Itoa(int(limit.Seconds())))
	r.py.Stderr = r.tail
	r.py.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUNBUFFERED=1")
	setProcAttr(r.py)
	r.cancelPy = cancelPy
	in, err := r.py.StdinPipe()
	if err != nil {
		r.kill()
		return nil, err
	}
	out, err := r.py.StdoutPipe()
	if err != nil {
		r.kill()
		return nil, err
	}
	if err := r.py.Start(); err != nil {
		r.kill()
		return nil, fmt.Errorf("启动 Python 失败: %v", err)
	}
	r.in, r.out = in, bufio.NewReader(out)
	var ready struct {
		Ready bool   `json:"ready"`
		Err   string `json:"err"`
	}
	if err := r.readReply(&ready, limit+10*time.Second); err != nil {
		r.kill()
		return nil, fmt.Errorf("常驻 LibreOffice 启动失败: %v%s", err, suffix(r.tail.String()))
	}
	if !ready.Ready {
		r.kill()
		return nil, errors.New(ready.Err)
	}
	logx.Debugf("常驻 LibreOffice 已启动（进程 %d，用时 %.1fs）", r.so.Process.Pid, time.Since(t0).Seconds())
	return r, nil
}

var errLOTimeout = errors.New("超时")

// readReply 读取桥接脚本的一行回复。
func (r *loResident) readReply(v any, timeout time.Duration) error {
	ch := make(chan error, 1)
	go func() {
		line, err := r.out.ReadBytes('\n')
		if err != nil {
			ch <- fmt.Errorf("桥接进程已退出: %v", err)
			return
		}
		ch <- json.Unmarshal(line, v)
	}()
	select {
	case err := <-ch:
		return err
	case <-time.After(timeout):
		return errLOTimeout
	}
}

// kill 立即结束 soffice 和桥接脚本（连同子进程）。
func (r *loResident) kill() {
	if r == nil {
		return
	}
	if r.in != nil {
		r.in.Close()
	}
	r.cancelPy()
	r.cancelSo()
	if r.py != nil && r.py.Process != nil {
		_ = r.py.Wait()
	}
	if r.so.Process != nil {
		_ = r.so.Wait()
	}
}

// stop 正常结束：关闭桥接脚本的输入，脚本会让 LibreOffice 退出；10 秒内没退出就强制结束。
func (r *loResident) stop() {
	if r == nil {
		return
	}
	r.in.Close()
	done := make(chan struct{})
	go func() {
		_ = r.py.Wait()
		_ = r.so.Wait()
		close(done)
	}()
	select {
	case <-done:
		r.cancelPy()
		r.cancelSo()
	case <-time.After(10 * time.Second):
		r.cancelPy()
		r.cancelSo()
		<-done
	}
}

type loReply struct {
	OK   bool    `json:"ok"`
	Err  string  `json:"err"`
	Sec  float64 `json:"sec"`
	Dead bool    `json:"dead"`
}

// convert 转换一个文件。返回错误表示 LibreOffice 卡住或已退出，调用方应结束它。
func (r *loResident) convert(src, out, target string, wait time.Duration) (loReply, error) {
	var reply loReply
	req, _ := json.Marshal(map[string]string{"in": src, "out": out, "filter": loFilters[target]})
	if _, err := r.in.Write(append(req, '\n')); err != nil {
		return reply, fmt.Errorf("常驻 LibreOffice 已退出%s", suffix(r.tail.String()))
	}
	if err := r.readReply(&reply, wait); err != nil {
		if errors.Is(err, errLOTimeout) {
			return reply, fmt.Errorf("格式转换超时（%s）", wait.Round(time.Second))
		}
		return reply, fmt.Errorf("格式转换组件异常退出：%v%s", err, suffix(r.tail.String()))
	}
	r.n++
	return reply, nil
}

// checkResident 启动一个临时的常驻 LibreOffice 转换 src（RTF），用于组件自检。
func checkResident(src, dir string) (string, error) {
	loMu.Lock()
	defer loMu.Unlock()
	r, err := startLO(filepath.Join(dir, "resident-profile"), 90*time.Second)
	if loCur == nil && loScript != "" { // 主程序里没有常驻实例：脚本用完即删
		defer func() { _ = os.Remove(loScript); loScript = "" }()
	}
	if err != nil {
		return "", err
	}
	defer r.stop()
	out := filepath.Join(dir, "resident.docx")
	reply, err := r.convert(src, out, "docx", 2*time.Minute)
	if err != nil {
		r.kill()
		return "", err
	}
	if !reply.OK {
		return "", errors.New(reply.Err)
	}
	return out, nil
}

// ShutdownLO 结束常驻的 LibreOffice。解析子进程退出前调用。
func ShutdownLO() {
	loMu.Lock()
	defer loMu.Unlock()
	loCur.stop()
	loCur = nil
	if loScript != "" {
		_ = os.Remove(loScript)
		loScript = ""
	}
}

// convertResident 用常驻的 LibreOffice 把 src 转换成 out。ok 为 false 表示常驻方式不可用，
// 调用方应改为单独启动 LibreOffice。
func convertResident(src, out, target, profile string, deadline time.Time, opt Options) (handled bool, err error) {
	loMu.Lock()
	defer loMu.Unlock()
	if loDisabled != "" {
		return false, nil
	}
	if loCur == nil {
		opt.Progress("正在启动 LibreOffice")
		limit := time.Until(deadline) - 15*time.Second
		if limit > 90*time.Second {
			limit = 90 * time.Second
		}
		if limit < 20*time.Second {
			limit = 20 * time.Second
		}
		r, err := startLO(profile, limit)
		if err != nil {
			loDisabled = err.Error()
			log.Printf("常驻 LibreOffice 不可用，改为每个文件单独启动：%v", err)
			return false, nil
		}
		loCur = r
	}
	r := loCur
	opt.Progress("正在转换")
	wait := time.Until(deadline)
	if wait < 10*time.Second {
		wait = 10 * time.Second
	}
	reply, err := r.convert(src, out, target, wait)
	if err != nil {
		// 卡住或崩溃：结束整个 LibreOffice，下一个文件重新启动
		r.kill()
		loCur = nil
		return true, err
	}
	if reply.Dead || r.n >= loRecycleAfter {
		r.stop()
		loCur = nil
	}
	if !reply.OK {
		return true, errors.New(reply.Err)
	}
	logx.Debugf("常驻 LibreOffice 转换用时 %.2fs（第 %d 个）", reply.Sec, r.n)
	return true, nil
}
