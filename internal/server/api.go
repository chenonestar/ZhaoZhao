package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/store"
)

func (s *Server) routes(mux *http.ServeMux) {
	h := func(p string, f http.HandlerFunc) { mux.HandleFunc(p, f) }
	h("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": s.Actions.Version})
	})
	h("/api/search", s.handleSearch)
	h("/api/preview", s.handlePreview)
	h("/api/open", s.handleOpen)
	h("/api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.M.Status()) })
	h("/api/components/check", s.post(func(r *http.Request) (any, error) {
		s.M.CheckComponents()
		return map[string]bool{"ok": true}, nil
	}))
	h("/api/pause", s.post(func(r *http.Request) (any, error) {
		var b struct {
			Root int64 `json:"root"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Root != 0 {
			return ok(), s.M.PauseRoot(b.Root, true)
		}
		s.M.PauseAll()
		return ok(), nil
	}))
	h("/api/resume", s.post(func(r *http.Request) (any, error) {
		var b struct {
			Root int64 `json:"root"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Root != 0 {
			return ok(), s.M.PauseRoot(b.Root, false)
		}
		s.M.ResumeAll()
		return ok(), nil
	}))
	h("/api/roots/add", s.post(func(r *http.Request) (any, error) {
		var b struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		root, err := s.M.AddRoot(b.Path)
		if err == nil {
			log.Printf("加入索引根 %s", logx.P(b.Path))
		}
		return root, err
	}))
	h("/api/roots/remove", s.post(func(r *http.Request) (any, error) {
		var b struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		return ok(), s.M.RemoveRoot(b.ID)
	}))
	h("/api/roots/rescan", s.post(func(r *http.Request) (any, error) {
		var b struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.ID == 0 {
			s.M.RescanAll()
		} else {
			s.M.RescanRoot(b.ID)
		}
		return ok(), nil
	}))
	h("/api/roots/rebuild", s.post(func(r *http.Request) (any, error) {
		var b struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		return ok(), s.M.RebuildRoot(b.ID)
	}))
	h("/api/roots/opts", s.post(func(r *http.Request) (any, error) {
		var b struct {
			ID   int64          `json:"id"`
			Opts index.RootOpts `json:"opts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		return ok(), s.M.SetRootOpts(b.ID, b.Opts)
	}))
	h("/api/roots/get", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		rs, _ := s.M.DB.ListRoots()
		for _, x := range rs {
			if x.ID == id {
				writeJSON(w, x)
				return
			}
		}
		writeErr(w, 404, fmt.Errorf("索引根不存在"))
	})
	h("/api/volumes/delete", s.post(func(r *http.Request) (any, error) {
		var b struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		return ok(), s.M.DeleteVolume(b.ID)
	}))
	// 失败列表：按原因类别、扩展名、路径关键字筛选，按编号分批加载（每批 limit 个，after 为上一批最后一个的编号）
	h("/api/failed", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 200
		}
		fl, err := s.M.DB.ListProblems(problemFilter(q), after, limit+1)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		type item struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Path   string `json:"path"`
			Ext    string `json:"ext"`
			State  int    `json:"state"`
			Cat    string `json:"cat"`
			Reason string `json:"reason"`
		}
		more := len(fl) > limit
		if more {
			fl = fl[:limit]
		}
		out := struct {
			Items []item `json:"items"`
			More  bool   `json:"more"`
		}{Items: []item{}, More: more}
		for i := range fl {
			p, _, _ := s.M.Display(&fl[i].File)
			out.Items = append(out.Items, item{fl[i].ID, fl[i].Name, p, fl[i].Ext, fl[i].State, store.ProblemLabel(fl[i].Cat), fl[i].Err})
		}
		writeJSON(w, out)
	})
	h("/api/failed/summary", func(w http.ResponseWriter, r *http.Request) {
		f := problemFilter(r.URL.Query())
		cats, exts, err := s.M.DB.ProblemSummary(f)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		roots, err := s.M.ProblemRoots(f)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, map[string]any{"cats": cats, "exts": exts, "roots": roots})
	})
	// 按文件夹查看：某个索引根里，文件夹 dir 下一级子文件夹各有多少（dir 为空从索引根开始）
	h("/api/failed/folders", func(w http.ResponseWriter, r *http.Request) {
		dir, fs, here, err := s.M.ProblemFolders(problemFilter(r.URL.Query()))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if fs == nil {
			fs = []store.FolderCount{}
		}
		writeJSON(w, map[string]any{"dir": dir, "folders": fs, "here": here})
	})
	// 重试：给了 filter 就重试符合条件的全部文件，否则重试 ids（ids 为空：全部失败的）
	h("/api/failed/retry", s.post(func(r *http.Request) (any, error) {
		var b struct {
			IDs    []int64              `json:"ids"`
			Filter *store.ProblemFilter `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Filter != nil {
			n, err := s.M.RetryProblems(*b.Filter)
			return map[string]int64{"count": n}, err
		}
		return ok(), s.M.RetryFailed(b.IDs)
	}))
	h("/api/failed/export", s.post(func(r *http.Request) (any, error) {
		var f store.ProblemFilter
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			return nil, err
		}
		p, n, err := s.M.ExportProblems(f)
		if err != nil {
			return nil, err
		}
		if s.Actions.RevealFile != nil {
			_ = s.Actions.RevealFile(p)
		}
		return map[string]any{"path": p, "count": n}, nil
	}))
	// 排除所在文件夹（失败列表、搜索结果）：加入排除规则，文件夹里的文件立即移出索引
	h("/api/exclude/folders", s.post(func(r *http.Request) (any, error) {
		var b struct {
			IDs []int64 `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		fs, err := s.M.ExcludeFolders(b.IDs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"folders": fs}, nil
	}))
	h("/api/failed/exclude", s.post(func(r *http.Request) (any, error) {
		var b struct {
			IDs []int64 `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, err
		}
		return ok(), s.excludeFiles(b.IDs)
	}))
	h("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var c index.Config
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				writeErr(w, 400, err)
				return
			}
			old := s.M.Config()
			if err := s.M.SetConfig(c); err != nil {
				writeErr(w, 500, err)
				return
			}
			if old.Hotkey != c.Hotkey && s.Actions.ApplyHotkey != nil {
				s.Actions.ApplyHotkey(c.Hotkey) // 立即生效，不用重启
			}
		}
		writeJSON(w, s.M.Config())
	})
	h("/api/hotkey", func(w http.ResponseWriter, r *http.Request) {
		st := HotkeyState{Message: "不可用"}
		if s.Actions.Hotkey != nil {
			st = s.Actions.Hotkey()
		}
		writeJSON(w, st)
	})
	h("/api/ui/pending", func(w http.ResponseWriter, r *http.Request) {
		p, _ := s.pendingPanel.Swap("").(string)
		writeJSON(w, map[string]string{"panel": p})
	})
	h("/api/space/stats", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.M.SpaceStats()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, st)
	})
	h("/api/space/compact", s.post(func(r *http.Request) (any, error) {
		return ok(), s.M.StartCompact()
	}))
	h("/api/ocrbench", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.M.OCRBenchInfo()) })
	h("/api/ocrbench/rerun", s.post(func(r *http.Request) (any, error) {
		return ok(), s.M.RebenchOCR()
	}))
	h("/api/legacytest", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.M.LegacyTest()) })
	h("/api/legacytest/start", s.post(func(r *http.Request) (any, error) {
		var b struct {
			PerExt int `json:"perExt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		return ok(), s.M.StartLegacyTest(b.PerExt)
	}))
	h("/api/legacytest/cancel", s.post(func(r *http.Request) (any, error) {
		s.M.CancelLegacyTest()
		return ok(), nil
	}))
	// 导出诊断信息在后台进行（大索引库要几十秒）：界面轮询进度，完成后打开所在文件夹
	h("/api/diagnostics", s.post(func(r *http.Request) (any, error) {
		var b struct {
			Anonymize bool `json:"anonymize"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if s.Actions.Diagnostics == nil {
			return nil, fmt.Errorf("不支持导出诊断信息")
		}
		return s.startDiagnostics(b.Anonymize), nil
	}))
	h("/api/diagnostics/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.diagStatus()) })
	h("/api/config/defaults", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, index.DefaultConfig(s.M.DataDir)) })
	h("/api/notices", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		writeJSON(w, s.M.Notices(after))
	})
	h("/api/history", s.handleHistory)
	h("/api/saved", s.handleSaved)
	h("/api/fs", s.handleFS)
	h("/api/hide", s.post(func(r *http.Request) (any, error) {
		hidden := s.Actions.HideWindow != nil && s.Actions.HideWindow()
		return map[string]bool{"ok": true, "hidden": hidden}, nil
	}))
	h("/api/quit", s.post(func(r *http.Request) (any, error) {
		log.Printf("界面请求退出")
		if s.Actions.Quit != nil {
			go func() { time.Sleep(200 * time.Millisecond); s.Actions.Quit() }()
		}
		return ok(), nil
	}))
}

func ok() map[string]bool { return map[string]bool{"ok": true} }

// post 包装只接受 POST 的接口，统一错误输出。
func (s *Server) post(f func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("需要 POST"))
			return
		}
		t := time.Now()
		v, err := f(r)
		if err != nil {
			log.Printf("操作失败 %s（用时 %.1fs）：%v", r.URL.Path, time.Since(t).Seconds(), err)
			writeErr(w, 400, err)
			return
		}
		if d := time.Since(t); d > 2*time.Second {
			log.Printf("操作 %s 用时 %.1fs", r.URL.Path, d.Seconds())
		}
		writeJSON(w, v)
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	p := searchParams(r)
	resp, err := s.Engine.Search(r.Context(), p)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	pv, err := s.Engine.Preview(id, searchParams(r))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, pv)
}

// handleOpen 打开文件或所在文件夹。只接受索引中的文件 id，不接受任意路径。
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("需要 POST"))
		return
	}
	var b struct {
		ID     int64  `json:"id"`
		Action string `json:"action"` // open | folder
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, 400, err)
		return
	}
	f, err := s.M.DB.GetFile(b.ID)
	if err != nil || f == nil {
		writeErr(w, 404, fmt.Errorf("文件不在索引中"))
		return
	}
	p, err := s.M.OpenTarget(f)
	if err != nil {
		var msg = err.Error()
		if _, off := err.(*index.OfflineError); off {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "offline": true})
			return
		}
		writeErr(w, 404, err)
		return
	}
	switch b.Action {
	case "folder":
		err = s.Actions.RevealFile(p)
	default:
		if f.Inner != "" {
			if s.Actions.OpenInner == nil {
				writeErr(w, 501, fmt.Errorf("暂不支持打开压缩包或邮件内的文件"))
				return
			}
			var tmp string
			tmp, err = s.Actions.OpenInner(p, f.Inner)
			if err == nil {
				err = s.Actions.OpenFile(tmp)
			}
		} else {
			err = s.Actions.OpenFile(p)
		}
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": p, "readOnlyCopy": f.Inner != "" && b.Action != "folder"})
}

// excludeFiles 把失败文件加入排除规则并从索引移除（F-BG-11）。
func (s *Server) excludeFiles(ids []int64) error {
	cfg := s.M.Config()
	for _, id := range ids {
		f, err := s.M.DB.GetFile(id)
		if err != nil || f == nil {
			continue
		}
		p, _, _ := s.M.Display(f)
		if f.Inner != "" {
			p, _ = s.M.AbsPath(f.VolumeID, f.RelPath)
		}
		cfg.ExcludeGlobs = append(cfg.ExcludeGlobs, p)
	}
	if err := s.M.SetConfig(cfg); err != nil {
		return err
	}
	return s.M.DB.DeleteFiles(ids)
}

// ---- 搜索历史与常用搜索（P1） ----

type savedSearch struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	var hist []string
	_, _ = s.M.DB.GetJSON("history", &hist)
	switch r.Method {
	case http.MethodPost:
		var b struct {
			Query string `json:"query"`
			Clear bool   `json:"clear"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Clear {
			hist = nil
		} else if q := strings.TrimSpace(b.Query); q != "" {
			out := []string{q}
			for _, h := range hist {
				if h != q && len(out) < 100 {
					out = append(out, h)
				}
			}
			hist = out
		}
		if err := s.M.DB.SetJSON("history", hist); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	if hist == nil {
		hist = []string{}
	}
	writeJSON(w, hist)
}

func (s *Server) handleSaved(w http.ResponseWriter, r *http.Request) {
	var saved []savedSearch
	_, _ = s.M.DB.GetJSON("saved", &saved)
	if r.Method == http.MethodPost {
		var b struct {
			Name   string `json:"name"`
			Query  string `json:"query"`
			Delete bool   `json:"delete"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			writeErr(w, 400, err)
			return
		}
		out := saved[:0:0]
		for _, x := range saved {
			if x.Name != b.Name {
				out = append(out, x)
			}
		}
		if !b.Delete && b.Name != "" && strings.TrimSpace(b.Query) != "" {
			out = append(out, savedSearch{b.Name, b.Query})
		}
		saved = out
		if err := s.M.DB.SetJSON("saved", saved); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	if saved == nil {
		saved = []savedSearch{}
	}
	writeJSON(w, saved)
}

// handleFS 列出子目录，供界面选择索引根（浏览器无法弹出原生文件夹选择框）。
func (s *Server) handleFS(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	resp := struct {
		Path    string  `json:"path"`
		Parent  string  `json:"parent"`
		Entries []entry `json:"entries"`
	}{Entries: []entry{}}
	if p == "" {
		if runtime.GOOS == "windows" {
			for _, v := range s.M.Prov.List() {
				resp.Entries = append(resp.Entries, entry{v.Label + " (" + strings.TrimRight(v.Mount, `\`) + ")", v.Mount})
			}
			writeJSON(w, resp)
			return
		}
		home, _ := os.UserHomeDir()
		p = home
		if p == "" {
			p = "/"
		}
	}
	p = filepath.Clean(p)
	resp.Path = p
	if par := filepath.Dir(p); par != p {
		resp.Parent = par
	} else if runtime.GOOS == "windows" {
		resp.Parent = ""
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			resp.Entries = append(resp.Entries, entry{e.Name(), filepath.Join(p, e.Name())})
		}
	}
	sort.Slice(resp.Entries, func(i, j int) bool {
		return strings.ToLower(resp.Entries[i].Name) < strings.ToLower(resp.Entries[j].Name)
	})
	writeJSON(w, resp)
}

func problemFilter(q url.Values) store.ProblemFilter {
	root, _ := strconv.ParseInt(q.Get("root"), 10, 64)
	return store.ProblemFilter{Cat: q.Get("cat"), Ext: q.Get("ext"), Q: q.Get("q"), Root: root, Dir: q.Get("dir")}
}
