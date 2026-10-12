package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/search"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	data := t.TempDir()
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	m := index.New(db, volume.System(), nil, data)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	docs := t.TempDir()
	os.WriteFile(filepath.Join(docs, "合同.txt"), []byte("违约金条款 axolotl"), 0o644)
	eng := &search.Engine{DB: db, Res: m, Norm: m.Norm}
	var revealed string
	srv, err := New(m, eng, 0, "", Actions{
		Version:    "test",
		OpenFile:   func(p string) error { return nil },
		RevealFile: func(p string) error { revealed = p; return nil },
		HideWindow: func() bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Shutdown(); m.Stop(); db.Close(); _ = revealed })
	if _, err := m.AddRoot(docs); err != nil {
		t.Fatal(err)
	}
	return srv, docs
}

func TestAuthAndGuards(t *testing.T) {
	srv, _ := newTestServer(t)
	base := "http://" + srv.Addr
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// 没有令牌：接口拒绝，页面本身可以打开
	if r, _ := plain.Get(base + "/api/status"); r.StatusCode != 401 {
		t.Fatalf("无 cookie 应 401: %d", r.StatusCode)
	}
	if r, _ := plain.Get(base + "/"); r.StatusCode != 200 {
		t.Fatalf("首页: %d", r.StatusCode)
	}
	// 错误的令牌不会种 cookie
	r, _ := plain.Get(base + "/?t=wrong")
	if len(r.Cookies()) != 0 {
		t.Fatal("错误令牌不应种 cookie")
	}
	// 正确的令牌：种 cookie 并跳转
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if r, err := c.Get(srv.URL()); err != nil || r.StatusCode != 200 {
		t.Fatalf("%v %v", err, r)
	}
	u, _ := url.Parse(base)
	if len(jar.Cookies(u)) == 0 {
		t.Fatal("应种下 cookie")
	}
	if r, _ := c.Get(base + "/api/ping"); r.StatusCode != 200 {
		t.Fatalf("带 cookie 应 200: %d", r.StatusCode)
	}
	// 状态变更请求必须带自定义头（防跨站表单）
	req, _ := http.NewRequest("POST", base+"/api/pause", strings.NewReader("{}"))
	if r, _ := c.Do(req); r.StatusCode != 403 {
		t.Fatalf("缺少自定义头应 403: %d", r.StatusCode)
	}
	req, _ = http.NewRequest("POST", base+"/api/pause", strings.NewReader("{}"))
	req.Header.Set("X-ZhaoZhao", "1")
	if r, _ := c.Do(req); r.StatusCode != 200 {
		t.Fatalf("带头应 200: %d", r.StatusCode)
	}
	// Host 校验（防 DNS 重绑定）
	req, _ = http.NewRequest("GET", base+"/api/ping", nil)
	req.Host = "evil.example.com"
	if r, _ := c.Do(req); r.StatusCode != 403 {
		t.Fatalf("陌生 Host 应 403: %d", r.StatusCode)
	}
	if srv.Addr[:10] != "127.0.0.1:" {
		t.Fatalf("只应监听本机回环地址: %s", srv.Addr)
	}
}

func TestSearchAndOpenAPI(t *testing.T) {
	srv, docs := newTestServer(t)
	base := "http://" + srv.Addr
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	c.Get(srv.URL())
	get := func(path string, out any) int {
		r, err := c.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if out != nil {
			json.Unmarshal(b, out)
		}
		return r.StatusCode
	}
	post := func(path, body string, out any) int {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("X-ZhaoZhao", "1")
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if out != nil {
			json.Unmarshal(b, out)
		}
		return r.StatusCode
	}
	var resp search.Response
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		get("/api/search?q="+url.QueryEscape("axolotl"), &resp)
		if resp.Total == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if resp.Total != 1 || resp.Results[0].Name != "合同.txt" {
		t.Fatalf("%+v", resp)
	}
	id := resp.Results[0].ID
	var pv search.Preview
	if get("/api/preview?id="+itoa(id)+"&q=axolotl", &pv) != 200 || len(pv.Hits) != 1 {
		t.Fatalf("%+v", pv)
	}
	var op struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
	}
	if post("/api/open", `{"id":`+itoa(id)+`,"action":"open"}`, &op) != 200 || op.Path != filepath.Join(docs, "合同.txt") {
		t.Fatalf("%+v", op)
	}
	// 只接受索引里的文件 id，不接受任意路径
	if post("/api/open", `{"id":999999,"action":"open"}`, nil) != 404 {
		t.Fatal("未知 id 应 404")
	}
	// 语法错误返回 400 与可读信息
	var e struct{ Error string }
	if get("/api/search?q="+url.QueryEscape(`"没闭合`), &e) != 400 || !strings.Contains(e.Error, "引号") {
		t.Fatalf("%+v", e)
	}
	// 搜索历史与常用搜索
	var hist []string
	post("/api/history", `{"query":"axolotl"}`, &hist)
	if len(hist) != 1 || hist[0] != "axolotl" {
		t.Fatalf("%v", hist)
	}
	var saved []savedSearch
	post("/api/saved", `{"name":"我的","query":"违约金 -草稿"}`, &saved)
	if len(saved) != 1 {
		t.Fatalf("%v", saved)
	}
	post("/api/saved", `{"name":"我的","delete":true}`, &saved)
	if len(saved) != 0 {
		t.Fatalf("%v", saved)
	}
	// 目录浏览
	var fsl struct {
		Entries []struct{ Name string }
	}
	if get("/api/fs?path="+url.QueryEscape(filepath.Dir(docs)), &fsl) != 200 || len(fsl.Entries) == 0 {
		t.Fatalf("%+v", fsl)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
