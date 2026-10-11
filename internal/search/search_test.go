package search

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/store"
)

type stubRes struct{}

func (stubRes) Display(f *store.File) (string, bool, string) {
	return `D:\` + strings.ReplaceAll(f.RelPath, "/", `\`), true, "备份盘A"
}

type doc struct {
	rel, text string
	size      int64
	mtime     time.Time
}

func newEngine(t *testing.T, docs []doc) *Engine {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	o := norm.Options{Simplify: true}
	rid, _ := db.AddRoot("vol1", "")
	for _, d := range docs {
		name := filepath.Base(d.rel)
		ext := extract.Ext(name)
		f := &store.File{RootID: rid, VolumeID: "vol1", RelPath: d.rel, Name: name, Ext: ext, Kind: extract.KindOf(ext),
			Size: d.size, MTime: d.mtime.Unix(), State: store.StatePending}
		nameFTS := norm.FTSText(norm.Tokenize(o.Runes(name)))
		err := db.Tx(func(tx *sql.Tx) error {
			if err := store.UpsertFile(tx, f, nameFTS); err != nil {
				return err
			}
			fts := norm.FTSText(norm.Tokenize(o.Runes(d.text)))
			return store.PutResult(tx, f.ID, store.StateDone, "", "", false, &store.Text{Text: d.text, LineBased: true}, fts, nameFTS)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return &Engine{DB: db, Res: stubRes{}, Norm: func() norm.Options { return o }}
}

func names(t *testing.T, e *Engine, q string) []string {
	t.Helper()
	r, err := e.Search(context.Background(), Params{Query: q})
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	var out []string
	for _, x := range r.Results {
		out = append(out, x.Name)
	}
	sort.Strings(out)
	return out
}

func check(t *testing.T, e *Engine, q string, want ...string) {
	t.Helper()
	got := names(t, e, q)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%q\n got  %v\n want %v", q, got, want)
	}
}

var corpus = []doc{
	{"项目/合同.txt", "甲方：张三。乙方：李四。违约金为合同总额的百分之五。报销单据附后。", 100, time.Date(2025, 3, 1, 0, 0, 0, 0, time.Local)},
	{"项目/草稿报销.txt", "张三的报销草稿，包含草稿二字", 5000, time.Date(2024, 1, 5, 0, 0, 0, 0, time.Local)},
	{"周报.md", "本周完成合同审批流程。合同签订审批已完成。", 300, time.Date(2025, 5, 5, 0, 0, 0, 0, time.Local)},
	{"contacts.txt", "电话 13812345678 身份证 110101199001011234 备用 15900001111", 200, time.Date(2023, 1, 1, 0, 0, 0, 0, time.Local)},
	{"invoice.txt", "Invoice number 20250707 for the project. Quarterly Report finalised.", 150, time.Date(2025, 7, 7, 0, 0, 0, 0, time.Local)},
	{"全角.txt", "ＡＢＣ公司２０２５年預算，不可抗力条款", 80, time.Date(2025, 2, 2, 0, 0, 0, 0, time.Local)},
	{"其他/无关.log", "nothing here 完全无关", 10, time.Date(2022, 1, 1, 0, 0, 0, 0, time.Local)},
}

func TestSearchBasics(t *testing.T) {
	e := newEngine(t, corpus)
	check(t, e, "张", "合同.txt", "草稿报销.txt")         // 单个汉字
	check(t, e, "张三", "合同.txt", "草稿报销.txt")        // 两个字
	check(t, e, "三张")                              // 顺序不对不命中
	check(t, e, "合同 违约金", "合同.txt")                // AND
	check(t, e, "违约金 OR 草稿", "合同.txt", "草稿报销.txt") // OR
	check(t, e, "张三 报销 -草稿", "合同.txt")             // NOT
	check(t, e, "张三 报销 NOT 草稿", "合同.txt")          // NOT 关键字
	check(t, e, "(合同 OR 协议) 审批", "周报.md")          // 分组
	check(t, e, `"不可抗力条款"`, "全角.txt")              // 短语
	check(t, e, "合同", "合同.txt", "周报.md")           // 文件名也参与
	check(t, e, "abc公司", "全角.txt")                 // 全角半角、大小写
	check(t, e, "预算", "全角.txt")                    // 繁简
	check(t, e, "2025", "invoice.txt", "全角.txt")   // 全角数字
	check(t, e, "inv", "invoice.txt")              // 英文前缀
	check(t, e, `"invoice number"`, "invoice.txt") // 英文短语
	check(t, e, "报销", "合同.txt", "草稿报销.txt")        // 内容与文件名
	check(t, e, "name:报销", "草稿报销.txt")             // 仅文件名
	check(t, e, "-张三 合同审批", "周报.md")               // 开头的排除
	check(t, e, "合同 -审批", "合同.txt")
}

func TestSearchScope(t *testing.T) {
	e := newEngine(t, corpus)
	r, _ := e.Search(context.Background(), Params{Query: "报销", Scope: "name"})
	if len(r.Results) != 1 || r.Results[0].Name != "草稿报销.txt" {
		t.Errorf("name scope: %+v", r.Results)
	}
	r, _ = e.Search(context.Background(), Params{Query: "周报", Scope: "content"})
	if len(r.Results) != 0 {
		t.Errorf("content scope: %+v", r.Results)
	}
}

func TestSearchNear(t *testing.T) {
	e := newEngine(t, corpus)
	check(t, e, "甲方 NEAR/10 违约", "合同.txt")
	check(t, e, "违约 NEAR/10 甲方", "合同.txt")
	check(t, e, "甲方 NEAR/2 违约")
}

func TestSearchRegex(t *testing.T) {
	e := newEngine(t, corpus)
	check(t, e, `/1[3-9]\d{9}/`, "contacts.txt")
	check(t, e, `/\d{18}/`, "contacts.txt")
	check(t, e, `/张三.*报销/`, "草稿报销.txt", "合同.txt")
	check(t, e, `/(?s)甲方.*乙方/`, "合同.txt")
	r, _ := e.Search(context.Background(), Params{Query: `/1[3-9]\d{9}/`})
	if len(r.Results) != 1 || r.Results[0].Hits != 3 { // 18 位身份证中也含手机号形式的 11 位数字
		t.Errorf("regex hits: %+v", r.Results)
	}
	m := r.Results[0].Snippets[0].Marks
	if len(m) != 3 {
		t.Errorf("marks %v", m)
	}
	if _, err := e.Search(context.Background(), Params{Query: `/(a)\1/`}); err == nil {
		t.Error("反向引用应报错")
	}
}

func TestSearchFuzzy(t *testing.T) {
	e := newEngine(t, corpus)
	check(t, e, "合同审批~", "周报.md")            // 合同签订审批：合/同 紧邻，审批之间夹 2 字
	check(t, e, "合同審批~", "周报.md")            // 繁简归一后相同
	check(t, e, "quarterlt~", "invoice.txt") // 1 处差异
	check(t, e, "quarterlt")
	check(t, e, "张三审批~") // 夹字过多
}

func TestSearchFilters(t *testing.T) {
	e := newEngine(t, corpus)
	check(t, e, "ext:md", "周报.md")
	check(t, e, "type:文本 张三", "合同.txt", "草稿报销.txt")
	check(t, e, `path:项目 张三`, "合同.txt", "草稿报销.txt")
	check(t, e, `path:D:\项目\ 张三`, "合同.txt", "草稿报销.txt")
	check(t, e, "size:>1000 张三", "草稿报销.txt")
	check(t, e, "modified:2025-01..2025-06 合同", "合同.txt", "周报.md")
	check(t, e, "modified:2024 张三", "草稿报销.txt")
	check(t, e, "modified:<2022-06-01", "无关.log")
	check(t, e, "vol:备份盘A ext:log", "无关.log")
	check(t, e, "vol:别的盘 ext:log")
	check(t, e, "(ext:md OR ext:log)", "周报.md", "无关.log")
	if _, err := e.Search(context.Background(), Params{Query: "type:火星"}); err == nil {
		t.Error("未知类别应报错")
	}
}

func TestSnippetAndPreview(t *testing.T) {
	e := newEngine(t, corpus)
	r, _ := e.Search(context.Background(), Params{Query: "违约金"})
	if len(r.Results) != 1 {
		t.Fatal(r.Results)
	}
	sn := r.Results[0].Snippets
	if len(sn) != 1 || sn[0].Loc != "第1行" {
		t.Fatalf("%+v", sn)
	}
	rs := []rune(sn[0].Text)
	m := sn[0].Marks[0]
	if string(rs[m[0]:m[1]]) != "违约金" {
		t.Errorf("mark %q", string(rs[m[0]:m[1]]))
	}
	pv, err := e.Preview(r.Results[0].ID, Params{Query: "违约金 乙方"})
	if err != nil || len(pv.Hits) != 2 {
		t.Fatalf("%v %+v", err, pv)
	}
	if string([]rune(pv.Text)[pv.Hits[0].S:pv.Hits[0].E]) != "乙方" {
		t.Errorf("hit0")
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		k    int
		ok   bool
	}{{"report", "reprot", 1, false}, {"report", "repart", 1, true}, {"abcdefghi", "abcdefxhi", 2, true}, {"abc", "abcdef", 2, false}} {
		if withinEdit(c.a, c.b, c.k) != c.ok {
			t.Errorf("%v", c)
		}
	}
}

func TestEmptyAndSyntaxError(t *testing.T) {
	e := newEngine(t, corpus)
	r, err := e.Search(context.Background(), Params{Query: "  "})
	if err != nil || r.Total != 0 {
		t.Fatal(err, r)
	}
	if _, err := e.Search(context.Background(), Params{Query: `"abc`}); err == nil {
		t.Fatal("expected error")
	}
	check(t, e, "#")
}
