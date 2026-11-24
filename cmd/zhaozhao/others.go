package main

// 本机上别的找找：不同文件夹里的便携版、安装版各有各的数据目录，同一个数据目录由系统锁保证只运行一个（见
// instance_*.go），不同数据目录的却能同时运行，两套都建索引、抢 CPU 和硬盘，托盘里两个一样的图标，快捷键只有
// 先启动的能用。启动时找出正在运行的其他找找，让用户选：关掉它、切换到它、还是都运行。
//
// 便携版换新版本时常常解压到新文件夹，新文件夹里没有索引，要从头建（扫描件 OCR 要几个小时）。第一次在新数据
// 目录里运行时，如果找到旧的数据目录（刚关掉的那个，或以前用过的），问用户要不要把它搬过来接着用。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/server"
)

// otherInstance 是本机上正在运行的另一个找找。
type otherInstance struct {
	PID     int
	Exe     string
	DataDir string
	Version string
	URL     string // 带令牌的界面地址
	port    int
	token   string
}

// ver 是显示用的版本号，如「v0.2.9」。
func (o otherInstance) ver() string {
	if o.Version == "" {
		return "（版本不明）"
	}
	return "v" + strings.TrimPrefix(o.Version, "v")
}

// logLabel 是写进日志的名字（路径可以在导出诊断信息时换成代号）。
func (o otherInstance) logLabel() string {
	return o.ver() + "（" + logx.P(filepath.Dir(o.Exe)) + "）"
}

// label 是对话框里显示的名字，如「v0.2.9（E:\找找0.2.9）」。
func (o otherInstance) label() string { return fmt.Sprintf("%s（%s）", o.ver(), filepath.Dir(o.Exe)) }

// readInstance 读数据目录里的 instance.json：那个找找的进程号、界面端口和令牌（不管它是否还在运行）。
func readInstance(dataDir string) (otherInstance, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "instance.json"))
	if err != nil {
		return otherInstance{}, false
	}
	var in instanceInfo
	if json.Unmarshal(b, &in) != nil || in.Port == 0 {
		return otherInstance{}, false
	}
	return otherInstance{PID: in.PID, DataDir: dataDir, port: in.Port, token: in.Token,
		URL: fmt.Sprintf("http://127.0.0.1:%d/?t=%s", in.Port, in.Token)}, true
}

// pingInstance 问那个找找的版本号。它很忙时（刚开机、机械硬盘满负荷）可能好几秒才回答。
func pingInstance(o otherInstance, timeout time.Duration) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", o.port), nil)
	for _, c := range server.AuthCookies(o.port, o.token) {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("返回 %s", resp.Status)
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return "", err
	}
	return p.Version, nil
}

// instanceRunning 说明数据目录 dataDir 是否正被一个找找使用：instance.json 里的进程还在、是用这个数据目录的
// 找找程序；查不了进程时（不是 Windows，或用命令行参数指定了数据目录）看连不连得上。
func instanceRunning(dataDir string) bool {
	o, ok := readInstance(dataDir)
	if !ok {
		return false
	}
	if exe := pidExe(o.PID); exe != "" && samePath(dataDirOfExe(exe), dataDir) {
		return true
	}
	_, err := pingInstance(o, 2*time.Second)
	return err == nil
}

// quitInstance 请另一个找找退出，等它的进程结束（最多 20 秒）。没退出时返回原因。
func quitInstance(o otherInstance) (bool, string) {
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/quit", o.port), strings.NewReader("{}"))
	for _, c := range server.AuthCookies(o.port, o.token) {
		req.AddCookie(c)
	}
	req.Header.Set("X-ZhaoZhao", "1")
	req.Header.Set("Content-Type", "application/json")
	why := ""
	if resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err != nil {
		why = "请它退出时出错：" + err.Error()
	} else {
		resp.Body.Close()
		if resp.StatusCode != 200 {
			why = "请它退出时返回 " + resp.Status
		}
	}
	if waitProcessExit(o.PID, 20*time.Second) {
		return true, ""
	}
	if why == "" {
		why = "它 20 秒内没有退出"
	}
	return false, why
}

// dataDirOfExe 返回某个找找程序用的数据目录（和 resolveDataDir 的规则相同，不看命令行参数和环境变量）。
func dataDirOfExe(exe string) string {
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "portable.flag")); err == nil {
		return filepath.Join(filepath.Dir(exe), "data")
	}
	if l := os.Getenv("LOCALAPPDATA"); l != "" {
		return filepath.Join(l, "ZhaoZhao")
	}
	return ""
}

func samePath(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// ---- 用过的数据目录：记在 %LOCALAPPDATA%\ZhaoZhao\known-data.json（安装版的数据目录，便携版也写这里） ----

type knownDir struct {
	DataDir string    `json:"dataDir"`
	Exe     string    `json:"exe"`
	Version string    `json:"version"`
	Used    time.Time `json:"used"`
}

func knownPath() string {
	if l := os.Getenv("LOCALAPPDATA"); l != "" {
		return filepath.Join(l, "ZhaoZhao", "known-data.json")
	}
	return ""
}

func loadKnown() []knownDir {
	var k []knownDir
	if p := knownPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &k)
		}
	}
	return k
}

// rememberDataDir 记下这次用的数据目录（最多记 10 个，最近的在前）。
func rememberDataDir(dataDir, exe string) {
	p := knownPath()
	if p == "" {
		return
	}
	k := slices.DeleteFunc(loadKnown(), func(x knownDir) bool { return samePath(x.DataDir, dataDir) })
	k = append([]knownDir{{DataDir: dataDir, Exe: exe, Version: version, Used: time.Now()}}, k...)
	if len(k) > 10 {
		k = k[:10]
	}
	if b, err := json.MarshalIndent(k, "", " "); err == nil {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, b, 0o600)
	}
}

// hasIndex 说明数据目录里有没有索引库。
func hasIndex(dataDir string) bool { return indexSize(dataDir) > 0 }

// indexSize 是数据目录里索引库的大小（含还没写回的部分），没有索引库时为 0。
func indexSize(dataDir string) int64 {
	st, err := os.Stat(filepath.Join(dataDir, "index.db"))
	if err != nil || st.Size() == 0 {
		return 0
	}
	n := st.Size()
	if w, err := os.Stat(filepath.Join(dataDir, "index.db-wal")); err == nil {
		n += w.Size()
	}
	return n
}

// smallFactor：这里已有索引时，旧索引至少是它的这么多倍才问要不要换（这里的索引刚开始建、没什么内容）。
const smallFactor = 10

// declinedFile 记下用户在这个数据目录里选过「不换旧索引」，以后不再问。
const declinedFile = "adopt-declined"

// adoptCandidate 找一个可以搬过来接着用的旧数据目录：有索引库、和 dataDir 在同一个盘上（搬过来只是改名，
// 一瞬间；不同盘要复制几个 GB，不做）、没有找找在用。dataDir 里已有索引时，只找比它大得多的（这里的是刚开始
// 建的）。prefer 不为空时优先用它（刚关掉的那个找找的）。
func adoptCandidate(dataDir string, prefer *knownDir) (knownDir, bool) {
	cur := indexSize(dataDir)
	var c []knownDir
	if prefer != nil {
		c = append(c, *prefer)
	}
	c = append(c, loadKnown()...)
	for _, k := range c {
		n := indexSize(k.DataDir)
		if samePath(k.DataDir, dataDir) || n == 0 || (cur > 0 && n < smallFactor*cur) ||
			!strings.EqualFold(filepath.VolumeName(absPath(k.DataDir)), filepath.VolumeName(absPath(dataDir))) {
			continue
		}
		if instanceRunning(k.DataDir) {
			continue
		}
		return k, true
	}
	return knownDir{}, false
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// asideName 是换下来的数据目录改成的名字，如「data-换下-20261109-1029」。
func asideName(dir string, t time.Time) string {
	return filepath.Clean(dir) + "-换下-" + t.Format("20060102-1504")
}

// adoptDataDir 把旧数据目录 from 搬成 to（同一个盘上改名）。to 里已有东西时：aside 为空就报错；不为空就先把
// to 改名成 aside 留着（里面是刚开始建的小索引）。旧的找找刚退出时解析子进程可能还拿着文件，最多重试 10 秒。
//
// 安装版的数据目录（%LOCALAPPDATA%\ZhaoZhao）里放着用过的数据目录列表（known-data.json），搬之前先拿出来，
// 搬完放回去。
func adoptDataDir(from, to, aside string) error {
	var known []byte
	if ents, err := os.ReadDir(to); err == nil {
		kn := filepath.Base(knownPath())
		empty := true
		for _, e := range ents {
			if e.Name() != kn {
				empty = false
			}
		}
		if !empty && aside == "" {
			return fmt.Errorf("%s 不是空的", to)
		}
		kp := filepath.Join(to, kn)
		if b, err := os.ReadFile(kp); err == nil {
			known = b
			_ = os.Remove(kp)
		}
		if empty {
			err = os.Remove(to)
		} else {
			err = retryRename(to, aside)
		}
		if err != nil {
			if known != nil {
				_ = os.WriteFile(kp, known, 0o600)
			}
			return err
		}
	}
	defer func() {
		if known != nil {
			_ = os.WriteFile(filepath.Join(to, filepath.Base(knownPath())), known, 0o600)
		}
	}()
	err := retryRename(from, to)
	if err != nil {
		if aside != "" && retryRename(aside, to) == nil { // 换回来，接着用这里的索引
			return err
		}
		_ = os.MkdirAll(to, 0o700)
	}
	return err
}

// retryRename 改名，失败时（文件还被刚退出的程序拿着）最多重试 10 秒。
func retryRename(from, to string) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// askWait 是启动时等用户在对话框里选择的总时长（启动用时里不算这段）。
var askWait time.Duration

func ask(title, main, content string, buttons []string) int {
	t := time.Now()
	defer func() { askWait += time.Since(t) }()
	return askFunc(title, main, content, buttons)
}

var askFunc = askChoice // 测试时换掉

// checkOthers 在启动时处理本机上别的找找和旧数据目录。返回 false 表示这个找找不用再启动了（切换到了正在运行的）。
// 只在打开界面的启动方式下问（开机自启 --no-ui 时不弹窗，照常启动）。
func checkOthers(dataDir string) bool {
	var closed *knownDir // 刚关掉的找找的数据目录
	others := runningOthers(dataDir)
	for _, o := range others {
		startNotes = append(startNotes, fmt.Sprintf("找别的找找：%s 在运行（进程 %d）", o.logLabel(), o.PID))
	}
	for _, o := range others {
		me := "v" + strings.TrimPrefix(version, "v")
		switch ask("找找", fmt.Sprintf("已有一个找找在运行：%s", o.label()),
			"两个找找同时运行会同时建索引、抢 CPU 和硬盘，托盘里有两个一样的图标，全局快捷键只有先启动的那个能用。",
			[]string{fmt.Sprintf("关掉它，运行这个（%s）\n这个找找用自己文件夹里的索引", me),
				fmt.Sprintf("切换到正在运行的 %s\n不启动这个", o.ver()),
				"两个都运行\n比较不同版本时用"}) {
		case 0:
			startNotes = append(startNotes, "关掉了另一个找找 "+o.logLabel())
			if ok, why := quitInstance(o); !ok {
				startNotes = append(startNotes, why)
			}
			closed = &knownDir{DataDir: o.DataDir, Exe: o.Exe, Version: o.Version}
		case 2:
			startNotes = append(startNotes, "另一个找找 "+o.logLabel()+" 也在运行（用户选了两个都运行）")
		default: // 切换到正在运行的，或关掉了对话框
			_ = showOrLaunchUI(o.URL)
			return false
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, declinedFile)); err == nil {
		return true
	}
	k, ok := adoptCandidate(dataDir, closed)
	if !ok {
		return true
	}
	what := "以前用过的索引"
	if k.Version != "" {
		what = fmt.Sprintf("v%s 用过的索引", strings.TrimPrefix(k.Version, "v"))
	}
	when := "刚关掉"
	if !k.Used.IsZero() {
		when = "最后使用 " + k.Used.Format("2006-01-02 15:04")
	}
	cur, old := indexSize(dataDir), indexSize(k.DataDir)
	aside := ""
	var main, content string
	var buttons []string
	if cur == 0 {
		main = "这里还没有索引，要接着用" + what + "吗？"
		content = fmt.Sprintf("旧索引 %s，在 %s（%s）。搬过来不用重新建索引（扫描件要重新识别几个小时）；搬走后，旧文件夹里的找找下次运行时会从头建索引。",
			sizeText(old), k.DataDir, when)
		buttons = []string{"搬过来接着用\n同一个盘上改个名，一瞬间就好", "重新建索引\n比较不同版本的建索引速度时选这个，以后不再问"}
	} else {
		aside = asideName(dataDir, time.Now())
		main = "这里的索引才建了一点，要换成" + what + "吗？"
		content = fmt.Sprintf("这里的索引 %s；旧索引 %s，在 %s（%s）。换过来不用重新建索引（扫描件要重新识别几个小时）；这里的索引改名为 %s 留着，不要了可以删掉。搬走后，旧文件夹里的找找下次运行时会从头建索引。",
			sizeText(cur), sizeText(old), k.DataDir, when, filepath.Base(aside))
		buttons = []string{"换成旧索引\n同一个盘上改个名，一瞬间就好", "继续用这里的索引\n以后不再问"}
	}
	switch ask("找找", main, content, buttons) {
	case 0:
	case 1:
		_ = os.WriteFile(filepath.Join(dataDir, declinedFile), []byte(k.DataDir), 0o600)
		startNotes = append(startNotes, "没有搬来旧索引 "+logx.P(k.DataDir)+"（用户选了不搬，以后不再问）")
		return true
	default:
		return true
	}
	if err := adoptDataDir(k.DataDir, dataDir, aside); err != nil {
		showMessage("找找", "没能搬过来："+err.Error()+"\n\n将继续用这里的索引。")
		startNotes = append(startNotes, "没能搬来旧索引 "+logx.P(k.DataDir)+"："+err.Error())
		return true
	}
	_ = os.Remove(filepath.Join(dataDir, declinedFile)) // 旧目录里以前记下的不算
	startNotes = append(startNotes, fmt.Sprintf("搬来了旧索引 %s（%s）", logx.P(k.DataDir), sizeText(old)))
	if aside != "" {
		startNotes = append(startNotes, fmt.Sprintf("原来的索引（%s）改名为 %s", sizeText(cur), logx.P(aside)))
	}
	return true
}

// sizeText 把字节数写成「850 MB」这样。
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
}

// startNotes 是启动前处理别的找找、旧索引的经过（日志还没打开，打开后写进去）。
var startNotes []string
