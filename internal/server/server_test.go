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
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/search"
	"github.com/chenonestar/zhaozhao/internal/store"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	return newTestServerWith(t, nil)
}

// newTestServerWith 在索引根里另外放上 files（文件名 → 内容）。
func newTestServerWith(t *testing.T, files map[string]string) (*Server, string) {
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
	for name, body := range files {
		os.WriteFile(filepath.Join(docs, name), []byte(body), 0o644)
	}
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

// 失败列表：按原因统计、筛选、分批加载、导出 CSV、整类重试。
func TestFailedListAPI(t *testing.T) {
	srv, _ := newTestServerWith(t, map[string]string{
		"坏1.docx": "not a zip", "坏2.docx": "not a zip either", "坏3.xlsx": "garbage",
		"空.txt": "",
	})
	base := "http://" + srv.Addr
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	c.Get(srv.URL())
	do := func(method, path, body string, out any) int {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("X-ZhaoZhao", "1")
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		if out != nil {
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s: %v: %s", path, err, b)
			}
		}
		return r.StatusCode
	}
	type summary struct {
		Cats []store.ProblemCat `json:"cats"`
		Exts []store.ExtCount   `json:"exts"`
	}
	count := func(s summary, key string) int64 {
		for _, c := range s.Cats {
			if c.Key == key {
				return c.Count
			}
		}
		return -1
	}
	var sum summary
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		do("GET", "/api/failed/summary", "", &sum)
		if count(sum, "corrupt") == 3 && count(sum, "meta_empty") == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if count(sum, "corrupt") != 3 || count(sum, "meta_empty") != 1 {
		t.Fatalf("类别统计: %+v", sum.Cats)
	}
	if len(sum.Exts) != 2 || sum.Exts[0] != (store.ExtCount{Ext: "docx", Count: 2}) {
		t.Fatalf("扩展名统计: %+v", sum.Exts)
	}
	type page struct {
		Items []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
			Cat  string `json:"cat"`
		} `json:"items"`
		More bool `json:"more"`
	}
	var p1, p2 page
	do("GET", "/api/failed?cat=corrupt&limit=2", "", &p1)
	if len(p1.Items) != 2 || !p1.More || p1.Items[0].Cat != "文件损坏或格式不对" {
		t.Fatalf("第一批: %+v", p1)
	}
	do("GET", "/api/failed?cat=corrupt&limit=2&after="+itoa(p1.Items[1].ID), "", &p2)
	if len(p2.Items) != 1 || p2.More {
		t.Fatalf("第二批: %+v", p2)
	}
	do("GET", "/api/failed?ext=docx&q="+url.QueryEscape("坏2"), "", &p1)
	if len(p1.Items) != 1 || !strings.HasSuffix(p1.Items[0].Path, "坏2.docx") {
		t.Fatalf("按扩展名和关键字: %+v", p1)
	}
	var ex struct {
		Path  string `json:"path"`
		Count int    `json:"count"`
	}
	if do("POST", "/api/failed/export", `{"cat":"corrupt","ext":"docx"}`, &ex) != 200 || ex.Count != 2 {
		t.Fatalf("导出: %+v", ex)
	}
	b, err := os.ReadFile(ex.Path)
	if err != nil || !strings.HasPrefix(string(b), "\xef\xbb\xbf路径,状态,原因类别") || strings.Count(string(b), "\r\n") != 3 ||
		!strings.Contains(string(b), "坏1.docx") || strings.Contains(string(b), "坏3.xlsx") {
		t.Fatalf("CSV 内容不对（%v）:\n%s", err, b)
	}
	var rt struct {
		Count int `json:"count"`
	}
	if do("POST", "/api/failed/retry", `{"filter":{"cat":"corrupt","ext":"xlsx"}}`, &rt) != 200 || rt.Count != 1 {
		t.Fatalf("整类重试: %+v", rt)
	}
}

// 导出诊断信息在后台进行：开始后立即返回，界面轮询进度；进行中再点不会重复开始；完成后打开所在文件夹。
func TestDiagnosticsAsync(t *testing.T) {
	release := make(chan struct{})
	var calls, revealed atomic.Int32
	s := &Server{Actions: Actions{
		Diagnostics: func(anon bool, progress func(string)) (string, error) {
			calls.Add(1)
			progress("日志 1/3")
			<-release
			return "/data/zhaozhao-diagnostics.zip", nil
		},
		RevealFile: func(string) error { revealed.Add(1); return nil },
	}}
	if st := s.startDiagnostics(true); !st.Running {
		t.Fatalf("%+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.diagStatus().Step != "日志 1/3" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := s.startDiagnostics(true); !st.Running || st.Step != "日志 1/3" {
		t.Fatalf("进行中应返回进度：%+v", st)
	}
	close(release)
	for s.diagStatus().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := s.diagStatus(); st.Running || st.Path != "/data/zhaozhao-diagnostics.zip" || st.Err != "" || calls.Load() != 1 {
		t.Fatalf("%+v，导出 %d 次", st, calls.Load())
	}
	for revealed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if revealed.Load() != 1 {
		t.Fatal("完成后应打开所在文件夹")
	}
}
