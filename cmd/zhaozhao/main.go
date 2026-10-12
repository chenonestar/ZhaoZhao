// 找找（ZhaoZhao）：Windows 单机全文检索工具。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/search"
	"github.com/chenonestar/zhaozhao/internal/server"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

var version = "0.1.1"

type instanceInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func main() {
	// 子进程模式：只做文件内容提取（崩溃或卡死不影响主程序）
	if len(os.Args) > 1 && os.Args[1] == worker.FlagName {
		worker.Serve()
		return
	}
	var (
		dataFlag = flag.String("data", "", "数据目录（默认 %LOCALAPPDATA%\\ZhaoZhao）")
		port     = flag.Int("port", 0, "界面服务端口（默认优先 17891，被占用时随机）")
		noUI     = flag.Bool("no-ui", false, "启动时不自动打开界面窗口")
		showVer  = flag.Bool("version", false, "显示版本")
		addRoots multiFlag
	)
	flag.Var(&addRoots, "add", "启动后添加索引根（可重复）")
	flag.Parse()
	if *showVer {
		fmt.Println("找找 ZhaoZhao", version)
		return
	}

	dataDir := resolveDataDir(*dataFlag)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fatalf("无法创建数据目录 %s: %v", dataDir, err)
	}
	var logw *logx.Rotating
	if lw, err := logx.New(filepath.Join(dataDir, "zhaozhao.log"), 5<<20, 3); err == nil {
		log.SetOutput(lw)
		logw = lw
	}
	suppressErrorDialogs()
	log.Printf("找找 %s 启动，数据目录 %s", version, dataDir)

	// 单实例：已有实例在运行则直接呼出它的窗口
	if url, ok := existingInstance(dataDir); ok {
		log.Printf("已有实例在运行，呼出窗口")
		if !*noUI {
			_ = showOrLaunchUI(url)
		}
		return
	}

	cleanStaleTemp()
	db, err := openDB(dataDir)
	if err != nil {
		fatalf("无法打开索引库: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		fatalf("%v", err)
	}
	nw := runtime.NumCPU()
	pool := worker.NewPool(exe, nw)
	mgr := index.New(db, volume.System(), pool, dataDir)
	ow := mgr.Config().OCRWorkers
	if ow < 1 {
		ow = 1
	}
	mgr.OCRPool = worker.NewPool(exe, ow)
	if err := mgr.Start(); err != nil {
		fatalf("启动索引服务失败: %v", err)
	}
	mgr.CheckComponents()

	eng := &search.Engine{DB: db, Res: mgr, Norm: mgr.Norm}
	tmp := newTempCache()
	quit := make(chan struct{}, 1)
	var srv *server.Server
	act := server.Actions{
		Version:    version,
		OpenFile:   openFile,
		RevealFile: revealFile,
		ShowWindow: func() { _ = showOrLaunchUI(srv.URL()) },
		HideWindow: hideUI,
		Quit: func() {
			select {
			case quit <- struct{}{}:
			default:
			}
		},
		OpenInner: func(container, inner string) (string, error) { return tmp.Open(container, inner, mgr.Config()) },
	}
	// 端口和令牌尽量保持不变：程序重启后，已经打开的界面窗口刷新一下就能继续用
	srv, err = server.New(mgr, eng, pickPort(*port), loadSecret(dataDir), act)
	if err != nil {
		fatalf("无法启动界面服务: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && err != http.ErrServerClosed {
			log.Printf("界面服务退出: %v", err)
		}
	}()
	writeInstance(dataDir, srv)

	for _, p := range addRoots {
		if _, err := mgr.AddRoot(p); err != nil {
			log.Printf("添加索引根 %s 失败: %v", p, err)
		}
	}
	fmt.Println("找找已启动：", srv.URL())
	if !*noUI {
		_ = launchUI(srv.URL())
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
		case <-quit:
		}
		shutdownShell()
	}()
	runShell(&shellCtx{URL: srv.URL(), Mgr: mgr, Hotkey: mgr.Config().Hotkey, Quit: act.Quit})

	// 退出：先让新实例可以启动、关掉界面窗口，再停止后台任务。
	// 任何一步卡住（例如网络盘上的目录读取）都不能让程序退不出去：最多等 8 秒。
	log.Printf("正在退出…")
	_ = os.Remove(filepath.Join(dataDir, "instance.json"))
	closeUI()
	done := make(chan struct{})
	go func() {
		srv.Shutdown()
		mgr.Stop() // 结束解析子进程（连同 LibreOffice、OCR）
		tmp.Cleanup()
		db.Close()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("找找退出")
	case <-time.After(8 * time.Second):
		log.Printf("退出超时，强制结束（子进程随作业对象一并结束）")
	}
	if logw != nil {
		logw.Close()
	}
	os.Exit(0)
}

// pickPort 返回要监听的端口：指定了就用指定的，否则优先固定端口，被占用时由系统分配。
func pickPort(flagPort int) int {
	if flagPort != 0 {
		return flagPort
	}
	const preferred = 17891
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred)); err == nil {
		ln.Close()
		return preferred
	}
	return 0
}

// loadSecret 读取（或生成）数据目录里的访问令牌。令牌保持不变，重启后旧窗口的登录状态仍然有效。
func loadSecret(dataDir string) string {
	p := filepath.Join(dataDir, "secret")
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			return s
		}
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	s := hex.EncodeToString(buf)
	_ = os.WriteFile(p, []byte(s), 0o600)
	return s
}

// cleanStaleTemp 删除上次异常退出遗留的临时目录（LibreOffice 配置、转换与 OCR 中间文件）。
func cleanStaleTemp() {
	ents, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() || !strings.HasPrefix(n, "zhaozhao-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.RemoveAll(filepath.Join(os.TempDir(), n))
		}
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint(*m) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func fatalf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Print(msg)
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func resolveDataDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if d := os.Getenv("ZHAOZHAO_DATA"); d != "" {
		return d
	}
	// 便携版：程序旁边有 portable.flag 时，数据放在程序目录下的 data 文件夹
	if exe, err := os.Executable(); err == nil {
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "portable.flag")); err == nil {
			return filepath.Join(filepath.Dir(exe), "data")
		}
	}
	if runtime.GOOS == "windows" {
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			return filepath.Join(l, "ZhaoZhao")
		}
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "zhaozhao")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "zhaozhao")
}

// openDB 打开索引库；检测到损坏时把旧库改名保留，建立新库并恢复索引根列表（F-BG-12）。
func openDB(dataDir string) (*store.DB, error) {
	db, err := store.Open(dataDir)
	if err == nil {
		if cerr := db.Check(); cerr == nil {
			return db, nil
		} else {
			log.Printf("索引库自检失败: %v", cerr)
		}
		db.Close()
	} else {
		log.Printf("打开索引库失败: %v", err)
	}
	ts := time.Now().Format("20060102-150405")
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Rename(filepath.Join(dataDir, "index.db"+suf), filepath.Join(dataDir, "index.db.corrupt-"+ts+suf))
	}
	db, err = store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	n := index.RestoreRoots(db, dataDir)
	log.Printf("已重建索引库，恢复了 %d 个索引根，将重新建立索引", n)
	return db, nil
}

func writeInstance(dataDir string, srv *server.Server) {
	var port int
	fmt.Sscanf(srv.Addr[len("127.0.0.1:"):], "%d", &port)
	b, _ := json.Marshal(instanceInfo{Port: port, Token: srv.Token, PID: os.Getpid()})
	_ = os.WriteFile(filepath.Join(dataDir, "instance.json"), b, 0o600)
}

// existingInstance 检查是否已有实例在运行；有则返回其界面地址。
func existingInstance(dataDir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "instance.json"))
	if err != nil {
		return "", false
	}
	var in instanceInfo
	if json.Unmarshal(b, &in) != nil || in.Port == 0 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d", in.Port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/ping", nil)
	req.AddCookie(&http.Cookie{Name: "zz", Value: in.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	return url + "/?t=" + in.Token, true
}
