// Package server 提供界面使用的本机 HTTP 接口，并托管内嵌的 React 界面。
// 只监听 127.0.0.1，并用随机令牌校验，防止浏览器中的其他网页调用。
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/search"
	"github.com/chenonestar/zhaozhao/internal/web"
)

// Server 是界面后端。
type Server struct {
	M      *index.Manager
	Engine *search.Engine
	Token  string
	Addr   string // 实际监听地址 127.0.0.1:port

	// Actions 是需要平台能力的回调（由 main 注入）。
	Actions Actions

	srv *http.Server
	ln  net.Listener

	pendingPanel atomic.Value // 托盘菜单要求界面打开的面板（如 "index"），界面取走后清空

	diagMu sync.Mutex
	diag   DiagState
}

// DiagState 是正在进行或最近一次的诊断信息导出。
type DiagState struct {
	Running bool   `json:"running"`
	Step    string `json:"step,omitempty"`    // 正在做哪一步
	Seconds int    `json:"seconds,omitempty"` // 已用秒数
	Path    string `json:"path,omitempty"`    // 生成的文件
	Err     string `json:"err,omitempty"`

	started time.Time
}

// startDiagnostics 在后台开始导出（已经在导出时不重复开始），返回当前状态。
func (s *Server) startDiagnostics(anonymize bool) DiagState {
	s.diagMu.Lock()
	if !s.diag.Running {
		s.diag = DiagState{Running: true, Step: "开始", started: time.Now()}
		go func() {
			p, err := s.Actions.Diagnostics(anonymize, func(step string) {
				s.diagMu.Lock()
				s.diag.Step = step
				s.diagMu.Unlock()
			})
			s.diagMu.Lock()
			s.diag.Running, s.diag.Step, s.diag.Path = false, "", p
			if err != nil {
				s.diag.Err, s.diag.Path = err.Error(), ""
			}
			s.diagMu.Unlock()
			if err == nil && s.Actions.RevealFile != nil {
				_ = s.Actions.RevealFile(p)
			}
		}()
	}
	s.diagMu.Unlock()
	return s.diagStatus()
}

func (s *Server) diagStatus() DiagState {
	s.diagMu.Lock()
	defer s.diagMu.Unlock()
	d := s.diag
	if d.Running {
		d.Seconds = int(time.Since(d.started).Seconds())
	}
	return d
}

// RequestPanel 让界面下次检查时打开指定面板（托盘菜单「索引与设置」）。
func (s *Server) RequestPanel(panel string) { s.pendingPanel.Store(panel) }

// HotkeyState 是全局快捷键的状态。
type HotkeyState struct {
	Hotkey  string `json:"hotkey"`
	Active  bool   `json:"active"`
	Message string `json:"message"`
}

// Actions 抽象与操作系统相关的动作。
type Actions struct {
	OpenFile   func(path string) error // 用默认程序打开
	RevealFile func(path string) error // 打开所在文件夹并选中文件
	ShowWindow func()                  // 呼出搜索窗口
	HideWindow func() bool             // 隐藏搜索窗口，返回是否成功（失败时界面自行关闭）
	Quit       func()
	Version    string
	// OpenInner 把容器（压缩包、邮件）内的条目解出到临时目录并返回路径。
	OpenInner func(containerPath, inner string) (string, error)
	// ApplyHotkey 立即启用新的全局快捷键（空表示不使用），返回结果；Hotkey 返回当前状态。
	ApplyHotkey func(spec string) HotkeyState
	Hotkey      func() HotkeyState
	// Diagnostics 导出诊断信息，返回生成的文件路径；progress 报告进行到哪一步。
	Diagnostics func(anonymize bool, progress func(string)) (string, error)
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// New 创建服务。port 为 0 表示随机端口；token 为空时随机生成。
func New(m *index.Manager, eng *search.Engine, port int, token string, act Actions) (*Server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	if token == "" {
		token = newToken()
	}
	s := &Server{M: m, Engine: eng, Token: token, Addr: ln.Addr().String(), Actions: act, ln: ln}
	mux := http.NewServeMux()
	s.routes(mux)
	dist, err := fs.Sub(web.FS, "dist")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", s.page(http.FileServer(http.FS(dist))))
	s.srv = &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// URL 返回带令牌的界面地址。
func (s *Server) URL() string { return "http://" + s.Addr + "/?t=" + s.Token }

func (s *Server) Serve() error { return s.srv.Serve(s.ln) }

func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}

const cookieName = "zz"

// page 处理首次打开：URL 中带正确令牌时写入 Cookie 并跳转到干净地址。
func (s *Server) page(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1 {
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// guard 校验 Host（防 DNS 重绑定）和 API 令牌。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host != s.Addr && host != "localhost:"+portOf(s.Addr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			c, err := r.Cookie(cookieName)
			if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				// 状态变更请求必须带自定义头，浏览器跨站表单无法设置
				if r.Header.Get("X-ZhaoZhao") == "" {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func intParam(r *http.Request, k string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return v
	}
	return def
}

func searchParams(r *http.Request) search.Params {
	q := r.URL.Query()
	return search.Params{
		Query:         q.Get("q"),
		Scope:         q.Get("scope"),
		Sort:          q.Get("sort"),
		Offset:        intParam(r, "offset", 0),
		Limit:         intParam(r, "limit", 50),
		Fuzzy:         q.Get("fuzzy") == "1",
		CaseSensitive: q.Get("cs") == "1",
	}
}
