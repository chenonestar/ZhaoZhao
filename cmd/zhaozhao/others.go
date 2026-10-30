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
func (o otherInstance) logLabel() string { return o.ver() + "（" + logx.P(filepath.Dir(o.Exe)) + "）" }

// label 是对话框里显示的名字，如「v0.2.9（E:\找找0.2.9）」。
func (o otherInstance) label() string { return fmt.Sprintf("%s（%s）", o.ver(), filepath.Dir(o.Exe)) }

// probeInstance 读数据目录里的 instance.json，确认那个找找在运行，取它的版本号。
func probeInstance(dataDir string) (otherInstance, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "instance.json"))
	if err != nil {
		return otherInstance{}, false
	}
	var in instanceInfo
	if json.Unmarshal(b, &in) != nil || in.Port == 0 {
		return otherInstance{}, false
	}
	o := otherInstance{PID: in.PID, DataDir: dataDir, port: in.Port, token: in.Token}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", in.Port), nil)
	req.AddCookie(&http.Cookie{Name: "zz", Value: in.Token})
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return o, false
	}
	defer resp.Body.Close()
	var p struct {
		Version string `json:"version"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&p) != nil {
		return o, false
	}
	o.Version = p.Version
	o.URL = fmt.Sprintf("http://127.0.0.1:%d/?t=%s", in.Port, in.Token)
	return o, true
}

// quitInstance 请另一个找找退出，等它的进程结束（最多 20 秒）。
func quitInstance(o otherInstance) bool {
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/quit", o.port), strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "zz", Value: o.token})
	req.Header.Set("X-ZhaoZhao", "1")
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
	}
	return waitProcessExit(o.PID, 20*time.Second)
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
func hasIndex(dataDir string) bool {
	st, err := os.Stat(filepath.Join(dataDir, "index.db"))
	return err == nil && st.Size() > 0
}

// adoptCandidate 找一个可以搬过来接着用的旧数据目录：有索引库、和 dataDir 在同一个盘上（搬过来只是改名，
// 一瞬间；不同盘要复制几个 GB，不做）、没有找找在用。prefer 不为空时优先用它（刚关掉的那个找找的）。
func adoptCandidate(dataDir string, prefer *knownDir) (knownDir, bool) {
	var c []knownDir
	if prefer != nil {
		c = append(c, *prefer)
	}
	c = append(c, loadKnown()...)
	for _, k := range c {
		if samePath(k.DataDir, dataDir) || !hasIndex(k.DataDir) ||
			!strings.EqualFold(filepath.VolumeName(absPath(k.DataDir)), filepath.VolumeName(absPath(dataDir))) {
			continue
		}
		if _, running := probeInstance(k.DataDir); running {
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

// adoptDataDir 把旧数据目录 from 搬成 to（同一个盘上改名）。to 是刚建的空目录时先删掉。旧的找找刚退出时
// 解析子进程可能还拿着文件，最多重试 10 秒。
//
// 安装版的数据目录（%LOCALAPPDATA%\ZhaoZhao）里放着用过的数据目录列表（known-data.json），搬之前先拿出来，
// 搬完放回去。
func adoptDataDir(from, to string) error {
	var known []byte
	if ents, err := os.ReadDir(to); err == nil {
		for _, e := range ents {
			if e.Name() != filepath.Base(knownPath()) {
				return fmt.Errorf("%s 不是空的", to)
			}
		}
		kp := filepath.Join(to, filepath.Base(knownPath()))
		if b, err := os.ReadFile(kp); err == nil {
			known = b
			_ = os.Remove(kp)
		}
		if err := os.Remove(to); err != nil {
			return err
		}
	}
	defer func() {
		if known != nil {
			_ = os.WriteFile(filepath.Join(to, filepath.Base(knownPath())), known, 0o600)
		}
	}()
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = os.MkdirAll(to, 0o700)
	return err
}

// checkOthers 在启动时处理本机上别的找找和旧数据目录。返回 false 表示这个找找不用再启动了（切换到了正在运行的）。
// 只在打开界面的启动方式下问（开机自启 --no-ui 时不弹窗，照常启动）。
func checkOthers(dataDir string) bool {
	var closed *knownDir // 刚关掉的找找的数据目录
	for _, o := range runningOthers(dataDir) {
		me := "v" + strings.TrimPrefix(version, "v")
		switch askChoice("找找", fmt.Sprintf("已有一个找找在运行：%s", o.label()),
			"两个找找同时运行会同时建索引、抢 CPU 和硬盘，托盘里有两个一样的图标，全局快捷键只有先启动的那个能用。",
			[]string{fmt.Sprintf("关掉它，运行这个（%s）\n这个找找用自己文件夹里的索引", me),
				fmt.Sprintf("切换到正在运行的 %s\n不启动这个", o.ver()),
				"两个都运行\n比较不同版本时用"}) {
		case 0:
			startNotes = append(startNotes, "关掉了另一个找找 "+o.logLabel())
			if !quitInstance(o) {
				startNotes = append(startNotes, "它 20 秒内没有退出")
			}
			closed = &knownDir{DataDir: o.DataDir, Exe: o.Exe, Version: o.Version}
		case 2:
			startNotes = append(startNotes, "另一个找找 "+o.logLabel()+" 也在运行（用户选了两个都运行）")
		default: // 切换到正在运行的，或关掉了对话框
			_ = showOrLaunchUI(o.URL)
			return false
		}
	}
	if hasIndex(dataDir) {
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
	if askChoice("找找", "这里还没有索引，要接着用"+what+"吗？",
		fmt.Sprintf("旧索引在 %s。搬过来不用重新建索引（扫描件要重新识别几个小时）；搬走后，旧文件夹里的找找下次运行时会从头建索引。", k.DataDir),
		[]string{"搬过来接着用\n同一个盘上改个名，一瞬间就好", "重新建索引\n比较不同版本的建索引速度时选这个"}) != 0 {
		return true
	}
	if err := adoptDataDir(k.DataDir, dataDir); err != nil {
		showMessage("找找", "没能搬过来："+err.Error()+"\n\n将重新建索引。")
		startNotes = append(startNotes, "没能搬来旧索引 "+logx.P(k.DataDir)+"："+err.Error())
		return true
	}
	startNotes = append(startNotes, "搬来了旧索引 "+logx.P(k.DataDir))
	return true
}

// startNotes 是启动前处理别的找找、旧索引的经过（日志还没打开，打开后写进去）。
var startNotes []string
