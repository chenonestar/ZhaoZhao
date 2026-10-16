package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ---- 失败列表：按原因分类、按扩展名统计、分批加载 ----

// problemRule 是一类原因：状态为 state、err 匹配 like 中任意一个（LIKE 模式）。
// 同一状态下按顺序取第一个匹配的；like 为空的一类收下该状态其余的全部。
type problemRule struct {
	key, label string
	state      int
	like       []string
}

var problemRules = []problemRule{
	{"encrypted", "已加密", StateEncrypted, nil},
	{"network", "网络盘读取失败", StateFailed, []string{"无法从网络盘读取%"}},
	{"timeout", "超时", StateFailed, []string{"%超时%"}},
	{"access", "无法读取（权限/占用）", StateFailed, []string{"无权限访问%", "%access is denied%", "%permission denied%",
		"%being used by another process%", "%locked a portion of the file%", "%拒绝访问%", "%另一个程序正在使用%", "读取文件失败%"}},
	{"ocr", "OCR 失败", StateFailed, []string{"%OCR%", "%识别%"}},
	{"convert", "格式转换失败", StateFailed, []string{"%格式转换%", "%LibreOffice%"}},
	{"corrupt", "文件损坏或格式不对", StateFailed, []string{"%损坏%", "%不是有效%", "%无法解析%", "%无法解码%", "%解析异常%",
		"%格式不正确%", "%没有页面%", "%内置解析不支持%", "%不支持的压缩方式%", "%压缩比异常%", "%解压总量%", "无法读取:%", "%unexpected EOF%",
		"%not a valid%", "%invalid%", "%malformed%", "%corrupt%"}},
	{"other", "其他", StateFailed, nil},
	{"meta_size", "文件太大", StateMetaOnly, []string{"文件超过%", "文件过大%"}},
	{"meta_type", "不支持的格式", StateMetaOnly, []string{"暂不支持%", "非文本文件%", "不支持的文件类型%"}},
	{"meta_ocr", "图片未做 OCR", StateMetaOnly, []string{"%OCR%"}},
	{"meta_empty", "空文件", StateMetaOnly, []string{"空文件%"}},
	{"meta_rule", "按索引根设置不提取", StateMetaOnly, []string{"按该索引根的设置%"}},
	{"meta_cloud", "云盘文件未下载", StateMetaOnly, []string{"云盘%"}},
	{"meta_other", "其他", StateMetaOnly, nil},
}

// problemCat 是按 problemRules 求出原因类别的 SQL 表达式（模式都是代码里的常量，直接写进 SQL）。
var problemCat = func() string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, r := range problemRules {
		fmt.Fprintf(&b, " WHEN state=%d", r.state)
		if len(r.like) > 0 {
			b.WriteString(" AND (")
			for i, p := range r.like {
				if i > 0 {
					b.WriteString(" OR ")
				}
				fmt.Fprintf(&b, "err LIKE '%s'", strings.ReplaceAll(p, "'", "''"))
			}
			b.WriteString(")")
		}
		fmt.Fprintf(&b, " THEN '%s'", r.key)
	}
	b.WriteString(" ELSE '' END")
	return b.String()
}()

// ProblemFilter 是失败列表的筛选条件。
type ProblemFilter struct {
	Cat string `json:"cat"` // ""：失败和已加密；"meta"：全部仅文件名；其他：某一类原因
	Ext string `json:"ext"` // 扩展名；"."：没有扩展名；""：不限
	Q   string `json:"q"`   // 路径关键字
}

// ProblemCat 是一类原因及其文件数。Meta 表示属于「仅文件名」。
type ProblemCat struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Meta  bool   `json:"meta"`
	Count int64  `json:"count"`
}

// ExtCount 是一个扩展名的文件数（Ext 为 "." 表示没有扩展名）。
type ExtCount struct {
	Ext   string `json:"ext"`
	Count int64  `json:"count"`
}

func catState(key string) int {
	for _, r := range problemRules {
		if r.key == key {
			return r.state
		}
	}
	return -1
}

var drivePrefix = regexp.MustCompile(`^[A-Za-z]:/+`)

// pathLike 把用户输入的路径关键字转成匹配 rel_path 的 LIKE 模式（盘符去掉、反斜杠换成 /）。
func pathLike(q string) string {
	q = strings.ReplaceAll(strings.TrimSpace(q), `\`, "/")
	q = drivePrefix.ReplaceAllString(q, "")
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
}

// where 返回筛选条件。总是带 state>=2，好让查询用上部分索引 files_problem。
// withCat、withExt 为 false 时不按类别、扩展名筛选（统计各类别、各扩展名的数量时用）。
func (f ProblemFilter) where(withCat, withExt bool) (string, []any) {
	w := []string{"state>=2"}
	var args []any
	if withCat {
		switch f.Cat {
		case "":
			w = append(w, "state<4")
		case "meta":
			w = append(w, "state=4")
		default:
			w = append(w, problemCat+"=?")
			args = append(args, f.Cat)
		}
	}
	if withExt && f.Ext != "" {
		ext := f.Ext
		if ext == "." {
			ext = ""
		}
		w = append(w, "ext=?")
		args = append(args, ext)
	}
	if strings.TrimSpace(f.Q) != "" {
		p := pathLike(f.Q)
		w = append(w, `(rel_path LIKE ? ESCAPE '\' OR inner LIKE ? ESCAPE '\')`)
		args = append(args, p, p)
	}
	return strings.Join(w, " AND "), args
}

// inGroup：类别 key 属于筛选条件 cat 选中的范围。
func inGroup(cat, key string) bool {
	switch cat {
	case "":
		s := catState(key)
		return s == StateFailed || s == StateEncrypted
	case "meta":
		return catState(key) == StateMetaOnly
	}
	return cat == key
}

// ProblemSummary 统计各类原因和各扩展名的文件数：类别数按扩展名、关键字筛选，扩展名数按类别、关键字筛选。
// 类别按 problemRules 的顺序全部列出（数量可能为 0）；扩展名按数量从多到少。
func (d *DB) ProblemSummary(f ProblemFilter) ([]ProblemCat, []ExtCount, error) {
	w, args := f.where(false, false)
	rows, err := d.R.Query(`SELECT `+problemCat+` AS cat, ext, count(*) FROM files INDEXED BY files_problem WHERE `+w+` GROUP BY cat, ext`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byCat := map[string]int64{}
	byExt := map[string]int64{}
	for rows.Next() {
		var cat, ext string
		var n int64
		if err := rows.Scan(&cat, &ext, &n); err != nil {
			return nil, nil, err
		}
		e := ext
		if e == "" {
			e = "."
		}
		if f.Ext == "" || f.Ext == e {
			byCat[cat] += n
		}
		if inGroup(f.Cat, cat) {
			byExt[e] += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	cats := make([]ProblemCat, 0, len(problemRules))
	for _, r := range problemRules {
		cats = append(cats, ProblemCat{Key: r.key, Label: r.label, Meta: r.state == StateMetaOnly, Count: byCat[r.key]})
	}
	exts := make([]ExtCount, 0, len(byExt))
	for e, n := range byExt {
		exts = append(exts, ExtCount{e, n})
	}
	sort.Slice(exts, func(i, j int) bool {
		if exts[i].Count != exts[j].Count {
			return exts[i].Count > exts[j].Count
		}
		return exts[i].Ext < exts[j].Ext
	})
	return cats, exts, nil
}

// Problem 是失败列表里的一个文件及其原因类别。
type Problem struct {
	File
	Cat string
}

// ProblemLabel 返回原因类别的名称（仅文件名的带上前缀）。
func ProblemLabel(key string) string {
	for _, r := range problemRules {
		if r.key == key {
			if r.state == StateMetaOnly {
				return "仅文件名：" + r.label
			}
			return r.label
		}
	}
	return key
}

// catScanner 在文件各列之后多读一列原因类别。
type catScanner struct {
	s   scanner
	cat *string
}

func (c catScanner) Scan(dest ...any) error { return c.s.Scan(append(dest, c.cat)...) }

// ListProblems 按编号顺序取 id 大于 after 的最多 limit 个文件（「加载更多」用上一批最后一个的编号）。
func (d *DB) ListProblems(f ProblemFilter, after int64, limit int) ([]Problem, error) {
	w, args := f.where(true, true)
	args = append(args, after, limit)
	rows, err := d.R.Query(`SELECT `+fileCols+`, `+problemCat+` FROM files INDEXED BY files_problem WHERE `+w+` AND id>? ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Problem
	for rows.Next() {
		var p Problem
		if p.File, err = scanFile(catScanner{rows, &p.Cat}); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EachProblem 按编号顺序逐个回调符合条件的文件（导出用）。
func (d *DB) EachProblem(f ProblemFilter, fn func(*Problem) error) error {
	var after int64
	for {
		batch, err := d.ListProblems(f, after, 1000)
		if err != nil {
			return err
		}
		for i := range batch {
			if err := fn(&batch[i]); err != nil {
				return err
			}
		}
		if len(batch) < 1000 {
			return nil
		}
		after = batch[len(batch)-1].ID
	}
}

// RetryProblems 把符合条件的失败、已加密文件重新排队（仅文件名的不动：放宽设置后重新扫描时会自动处理）。
// 压缩包、邮件里的条目不能单独提取：重新排队它所在的压缩包、邮件。返回重新排队的文件数。
func (d *DB) RetryProblems(f ProblemFilter) (int64, error) {
	w, args := f.where(true, true)
	return d.retryWhere(w+" AND state IN (2,3)", args)
}

// RetryFailed 把指定的失败、已加密文件重新排队；ids 为空时重试所有失败文件（不含已加密）。
func (d *DB) RetryFailed(ids []int64) error {
	if len(ids) == 0 {
		_, err := d.retryWhere("state=2", nil)
		return err
	}
	for len(ids) > 0 {
		n := min(len(ids), 500)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		if _, err := d.retryWhere(`state IN (2,3) AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", n), ",")+`)`, args); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

func (d *DB) retryWhere(cond string, args []any) (int64, error) {
	var total int64
	err := d.Tx(func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE files SET state=0, err='' WHERE inner='' AND state<>0 AND (volume_id, rel_path) IN
			(SELECT volume_id, rel_path FROM files WHERE `+cond+` AND inner<>'')`, args...)
		if err != nil {
			return err
		}
		n1, _ := r.RowsAffected()
		r, err = tx.Exec(`UPDATE files SET state=0, err='' WHERE `+cond+` AND inner=''`, args...)
		if err != nil {
			return err
		}
		n2, _ := r.RowsAffected()
		total = n1 + n2
		return nil
	})
	return total, err
}
