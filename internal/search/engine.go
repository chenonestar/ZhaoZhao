package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/query"
	"github.com/chenonestar/zhaozhao/internal/store"
)

// Resolver 把存储的「卷 + 相对路径」换算成当前可显示的路径。
type Resolver interface {
	// Display 返回完整显示路径（容器内文件形如 "D:\a.zip › 合同.docx"）、卷是否在线、卷标。
	Display(f *store.File) (path string, online bool, volLabel string)
}

// Engine 是搜索入口。
type Engine struct {
	DB   *store.DB
	Res  Resolver
	Norm func() norm.Options
}

// Params 是一次搜索的参数。
type Params struct {
	Query         string `json:"query"`
	Scope         string `json:"scope"` // all（默认）| content | name
	Sort          string `json:"sort"`  // relevance（默认）| mtime | name | path
	Offset        int    `json:"offset"`
	Limit         int    `json:"limit"`
	Fuzzy         bool   `json:"fuzzy"`         // 全局模糊开关
	CaseSensitive bool   `json:"caseSensitive"` // 正则区分大小写
	MaxVerify     int    `json:"-"`
}

// Result 是结果列表里的一项。
type Result struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Ext       string    `json:"ext"`
	Kind      string    `json:"kind"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	MTime     int64     `json:"mtime"`
	State     int       `json:"state"`
	Err       string    `json:"err,omitempty"`
	OCR       bool      `json:"ocr,omitempty"`
	Online    bool      `json:"online"`
	VolLabel  string    `json:"volLabel,omitempty"`
	Score     float64   `json:"score"`
	Hits      int       `json:"hits"`
	Snippets  []Snippet `json:"snippets,omitempty"`
	NameMarks [][2]int  `json:"nameMarks,omitempty"`
	Dups      []string  `json:"dups,omitempty"`
	Fuzzy     bool      `json:"fuzzy,omitempty"`
}

// Response 是搜索结果。
type Response struct {
	Total     int      `json:"total"`
	Results   []Result `json:"results"`
	ElapsedMs int64    `json:"elapsedMs"`
	Truncated bool     `json:"truncated,omitempty"` // 候选过多，只校验了一部分
	Exact     bool     `json:"exact"`               // 结果完全来自索引（未做逐文件校验）
}

type cand struct {
	store.Ranked
	path      string
	online    bool
	volLabel  string
	score     float64
	hits      int
	fuzzy     bool
	dups      []string
	needsText bool
}

func hasNode(n query.Node, pred func(query.Node) bool) bool {
	if pred(n) {
		return true
	}
	switch t := n.(type) {
	case *query.And:
		for _, c := range t.Items {
			if hasNode(c, pred) {
				return true
			}
		}
	case *query.Or:
		for _, c := range t.Items {
			if hasNode(c, pred) {
				return true
			}
		}
	case *query.Not:
		return hasNode(t.Item, pred)
	}
	return false
}

func (e *Engine) newMeta(f *store.File, o norm.Options, now time.Time) *metaEval {
	path, _, label := e.Res.Display(f)
	return &metaEval{now: now, norm: o, path: path, volLabel: label, file: f}
}

// Search 执行查询。
func (e *Engine) Search(ctx context.Context, p Params) (*Response, error) {
	start := time.Now()
	ast, err := query.Parse(p.Query)
	if err != nil {
		return nil, fmt.Errorf("查询语法错误：%w", err)
	}
	resp := &Response{Results: []Result{}}
	if ast == nil {
		return resp, nil
	}
	if err := validate(ast); err != nil {
		return nil, err
	}
	if p.Scope == "" {
		p.Scope = "all"
	}
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.MaxVerify <= 0 {
		p.MaxVerify = 200000
	}
	o := e.Norm()
	pl := &planner{db: e.DB, norm: o, scope: p.Scope, fuzzyAll: p.Fuzzy}
	plan := pl.plan(ast)
	if plan.kind == fxNone {
		resp.Exact = true
		resp.ElapsedMs = time.Since(start).Milliseconds()
		return resp, nil
	}
	match := ""
	if plan.kind == fxExpr {
		match = plan.s
	}
	ranked, err := e.DB.QueryRanked(match)
	if err != nil {
		return nil, fmt.Errorf("查询索引失败：%w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	now := time.Now()
	hasFilter := hasNode(ast, func(n query.Node) bool { _, ok := n.(*query.Filter); return ok })
	cands := make([]*cand, 0, len(ranked))
	for i := range ranked {
		c := &cand{Ranked: ranked[i]}
		c.path, c.online, c.volLabel = e.Res.Display(&c.File)
		tri := triUnknown
		if hasFilter {
			me := &metaEval{now: now, norm: o, path: c.path, volLabel: c.volLabel, file: &c.File}
			tri = me.tri(ast)
			if tri == triFalse {
				continue
			}
		}
		cands = append(cands, c)
		if !plan.exact && tri != triTrue {
			c.needsText = true
		}
	}

	resp.Exact = plan.exact
	if !plan.exact {
		var needAll []*cand
		for _, c := range cands {
			if c.needsText {
				needAll = append(needAll, c)
			}
		}
		// 按索引相关度排序后截断，再逐个校验
		sort.SliceStable(needAll, func(i, j int) bool { return needAll[i].Rank < needAll[j].Rank })
		needText := needAll
		if len(needText) > p.MaxVerify {
			needText = needText[:p.MaxVerify]
			resp.Truncated = true
		}
		verified, err := e.verify(ctx, ast, p, o, now, needText)
		if err != nil {
			return nil, err
		}
		drop := make(map[*cand]bool, len(needAll))
		for _, c := range needAll {
			drop[c] = true // 未通过校验或因截断未校验的待校验项都丢弃
		}
		for _, c := range verified {
			delete(drop, c)
		}
		kept := cands[:0:0]
		for _, c := range cands {
			if !drop[c] {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	for _, c := range cands {
		if plan.exact {
			c.score = -c.Rank
		}
	}

	cands = groupDuplicates(cands)
	sortCands(cands, p.Sort)
	resp.Total = len(cands)
	if p.Offset > len(cands) {
		p.Offset = len(cands)
	}
	end := p.Offset + p.Limit
	if end > len(cands) {
		end = len(cands)
	}
	page := cands[p.Offset:end]
	resp.Results = make([]Result, 0, len(page))
	for _, c := range page {
		resp.Results = append(resp.Results, e.render(ast, p, o, now, c))
	}
	resp.ElapsedMs = time.Since(start).Milliseconds()
	return resp, nil
}

// verify 并行读取候选文本并逐个精确校验，返回通过的候选并写入分数。
func (e *Engine) verify(ctx context.Context, ast query.Node, p Params, o norm.Options, now time.Time, list []*cand) ([]*cand, error) {
	var mu sync.Mutex
	var out []*cand
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	ch := make(chan *cand)
	var wg sync.WaitGroup
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			regexes := map[string]*regexp.Regexp{}
			for c := range ch {
				if ctx.Err() != nil {
					continue
				}
				text := ""
				has := false
				if c.State == store.StateDone || c.TextLen > 0 {
					t, err := e.DB.GetText(c.ID)
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						continue
					}
					if t != nil {
						text, has = t.Text, true
					}
				}
				d := newDocView(o, c.Name, text, has)
				me := &metaEval{now: now, norm: o, path: c.path, volLabel: c.volLabel, file: &c.File}
				ev := &evaluator{o: o, scope: p.Scope, caseSens: p.CaseSensitive, fuzzyAll: p.Fuzzy, d: d, meta: me, regexes: regexes, collect: true}
				if !ev.eval(ast) {
					continue
				}
				c.hits = len(mergeSpans(ev.cspans)) + len(ev.nspans)
				c.fuzzy = ev.sawFuzzy && !ev.sawExact
				sc := -c.Rank + math.Log1p(float64(c.hits))
				if len(ev.nspans) > 0 {
					sc += 10
				}
				if c.fuzzy {
					sc -= 20
				}
				c.score = sc
				mu.Lock()
				out = append(out, c)
				mu.Unlock()
			}
		}()
	}
	for _, c := range list {
		if ctx.Err() != nil {
			break
		}
		ch <- c
	}
	close(ch)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, firstErr
}

// groupDuplicates 把内容完全相同（哈希与大小一致）的文件合并为一条（F-RES-10）。
func groupDuplicates(cs []*cand) []*cand {
	idx := map[string]*cand{}
	out := cs[:0:0]
	for _, c := range cs {
		if c.Hash == "" || c.State != store.StateDone {
			out = append(out, c)
			continue
		}
		key := fmt.Sprintf("%s/%d", c.Hash, c.Size)
		if m, ok := idx[key]; ok {
			m.dups = append(m.dups, c.path)
			continue
		}
		idx[key] = c
		out = append(out, c)
	}
	return out
}

func sortCands(cs []*cand, by string) {
	less := func(i, j int) bool { return cs[i].score > cs[j].score }
	switch by {
	case "mtime":
		less = func(i, j int) bool { return cs[i].MTime > cs[j].MTime }
	case "name":
		less = func(i, j int) bool { return strings.ToLower(cs[i].Name) < strings.ToLower(cs[j].Name) }
	case "path":
		less = func(i, j int) bool { return strings.ToLower(cs[i].path) < strings.ToLower(cs[j].path) }
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if by == "" || by == "relevance" {
			if cs[i].score != cs[j].score {
				return cs[i].score > cs[j].score
			}
			return cs[i].MTime > cs[j].MTime
		}
		return less(i, j)
	})
}

// render 为一页中的结果加载文本，生成片段与命中数。
func (e *Engine) render(ast query.Node, p Params, o norm.Options, now time.Time, c *cand) Result {
	r := Result{ID: c.ID, Name: c.Name, Ext: c.Ext, Kind: c.Kind, Path: c.path, Size: c.Size, MTime: c.MTime,
		State: c.State, Err: c.Err, OCR: c.OCR, Online: c.online, VolLabel: c.volLabel, Score: c.score, Hits: c.hits,
		Dups: c.dups, Fuzzy: c.fuzzy}
	var t *store.Text
	has := false
	if c.State == store.StateDone || c.TextLen > 0 {
		if tt, err := e.DB.GetText(c.ID); err == nil && tt != nil {
			t, has = tt, true
		}
	}
	d := newDocView(o, c.Name, textOf(t), has)
	me := &metaEval{now: now, norm: o, path: c.path, volLabel: c.volLabel, file: &c.File}
	ev := &evaluator{o: o, scope: p.Scope, caseSens: p.CaseSensitive, fuzzyAll: p.Fuzzy, d: d, meta: me, regexes: map[string]*regexp.Regexp{}, collect: true}
	ev.eval(ast)
	if has {
		// 片段与高亮基于原文字符（归一化保证与原文一一对应）
		r.Snippets = makeSnippets([]rune(t.Text), t, ev.cspans, 3)
		if n := len(mergeSpans(ev.cspans)); n > 0 {
			r.Hits = n
		}
	}
	if len(ev.nspans) > 0 {
		for _, s := range mergeSpans(ev.nspans) {
			r.NameMarks = append(r.NameMarks, [2]int{s.S, s.E})
		}
		if r.Hits == 0 {
			r.Hits = len(r.NameMarks)
		}
	}
	return r
}

func textOf(t *store.Text) string {
	if t == nil {
		return ""
	}
	return t.Text
}

// PreviewHit 是预览区中的一处命中。
type PreviewHit struct {
	S   int    `json:"s"`
	E   int    `json:"e"`
	Loc string `json:"loc,omitempty"`
}

// Preview 是预览区所需的数据：提取出的纯文本及全部命中位置。
type Preview struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	Path      string       `json:"path"`
	Text      string       `json:"text"`
	Truncated bool         `json:"truncated,omitempty"`
	Hits      []PreviewHit `json:"hits"`
	NameMarks [][2]int     `json:"nameMarks,omitempty"`
	OCR       bool         `json:"ocr,omitempty"`
	Online    bool         `json:"online"`
	State     int          `json:"state"`
	Err       string       `json:"err,omitempty"`
	Header    string       `json:"header,omitempty"` // 邮件头等
	Kind      string       `json:"kind"`
}

const maxPreviewRunes = 2_000_000

// Preview 返回某文件的预览数据；query 为空时不含命中。
func (e *Engine) Preview(id int64, p Params) (*Preview, error) {
	f, err := e.DB.GetFile(id)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("文件不在索引中")
	}
	path, online, label := e.Res.Display(f)
	pv := &Preview{ID: f.ID, Name: f.Name, Path: path, OCR: f.OCR, Online: online, State: f.State, Err: f.Err, Kind: f.Kind, Hits: []PreviewHit{}}
	t, err := e.DB.GetText(id)
	if err != nil {
		return nil, err
	}
	pv.Header = mailHeader(f)
	rs := []rune(textOf(t))
	if len(rs) > maxPreviewRunes {
		rs = rs[:maxPreviewRunes]
		pv.Truncated = true
	}
	pv.Text = string(rs)
	if strings.TrimSpace(p.Query) == "" {
		return pv, nil
	}
	ast, err := query.Parse(p.Query)
	if err != nil || ast == nil {
		return pv, nil
	}
	if p.Scope == "" {
		p.Scope = "all"
	}
	o := e.Norm()
	d := newDocView(o, f.Name, pv.Text, t != nil)
	me := &metaEval{now: time.Now(), norm: o, path: path, volLabel: label, file: f}
	ev := &evaluator{o: o, scope: p.Scope, caseSens: p.CaseSensitive, fuzzyAll: p.Fuzzy, d: d, meta: me, regexes: map[string]*regexp.Regexp{}, collect: true}
	ev.eval(ast)
	loc := &locator{t: t, rs: rs}
	for _, s := range mergeSpans(ev.cspans) {
		if len(pv.Hits) >= 5000 {
			break
		}
		pv.Hits = append(pv.Hits, PreviewHit{S: s.S, E: s.E, Loc: loc.label(s.S)})
	}
	for _, s := range mergeSpans(ev.nspans) {
		pv.NameMarks = append(pv.NameMarks, [2]int{s.S, s.E})
	}
	return pv, nil
}

func mailHeader(f *store.File) string {
	if f.Kind != "mail" || f.Meta == "" {
		return ""
	}
	var m struct {
		Subject, From, To, Date string
	}
	if json.Unmarshal([]byte(f.Meta), &m) != nil {
		return ""
	}
	var b strings.Builder
	for _, kv := range [][2]string{{"主题", m.Subject}, {"发件人", m.From}, {"收件人", m.To}, {"日期", m.Date}} {
		if kv[1] != "" {
			b.WriteString(kv[0] + "：" + kv[1] + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
