package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func problemDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rid, _ := db.AddRoot("v", "")
	files := []struct {
		rel, inner, ext string
		state           int
		err             string
	}{
		{"工作/a.xls", "", "xls", StateEncrypted, "文件已加密"},
		{"工作/b.xls", "", "xls", StateEncrypted, "文件已加密"},
		{"工作/c.doc", "", "doc", StateFailed, "格式转换超时（2m0s）"},
		{"工作/d.pdf", "", "pdf", StateFailed, "PDF 已损坏或无法解析: bad xref"},
		{"工作/e.docx", "", "docx", StateFailed, "读取文件失败: open e.docx: The process cannot access the file because it is being used by another process."},
		{"工作/f.ppt", "", "ppt", StateFailed, "格式转换失败：exit status 1"},
		{"个人/g.png", "", "png", StateFailed, "OCR 识别失败（代码 2）: x"},
		{"个人/h.pdf", "", "pdf", StateFailed, "无法从网络盘读取: 超时"},
		{"个人/i.txt", "", "txt", StateFailed, "已取消"},
		{"个人/big.mp4", "", "mp4", StateMetaOnly, "文件超过 100 MB，仅索引文件名"},
		{"个人/x.exe", "", "exe", StateMetaOnly, "非文本文件，仅索引文件名"},
		{"个人/noext", "", "", StateMetaOnly, "非文本文件，仅索引文件名"},
		{"个人/图标.png", "", "png", StateMetaOnly, "图片尺寸过小，已跳过 OCR"},
		{"包.zip", "照片.jpg", "jpg", StateMetaOnly, "压缩包内的图片不做 OCR"},
		{"个人/扫描.jpg", "", "jpg", StateMetaOnly, "OCR 未启用，仅索引文件名"},
		{"个人/ok.txt", "", "txt", StateDone, ""},
		{"包.zip", "", "zip", StateDone, ""},
		{"包.zip", "里面/坏.doc", "doc", StateFailed, "不是有效的 Word 文档"},
	}
	err = db.Tx(func(tx *sql.Tx) error {
		for _, x := range files {
			f := &File{RootID: rid, VolumeID: "v", RelPath: x.rel, Inner: x.inner, Name: x.rel, Ext: x.ext, Size: 1, State: x.state, Err: x.err, Prio: 1}
			if err := UpsertFile(tx, f, "f"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func catCounts(cats []ProblemCat) string {
	var b []string
	for _, c := range cats {
		if c.Count > 0 {
			b = append(b, fmt.Sprintf("%s=%d", c.Key, c.Count))
		}
	}
	return strings.Join(b, " ")
}

// 失败原因按类别统计、按扩展名统计，类别与扩展名互相筛选。
func TestProblemSummary(t *testing.T) {
	db := problemDB(t)
	cats, exts, err := db.ProblemSummary(ProblemFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := "encrypted=2 network=1 timeout=1 access=1 ocr=1 convert=1 corrupt=2 other=1 meta_size=1 meta_type=2 meta_small=1 meta_inzip=1 meta_ocr=1"
	if got := catCounts(cats); got != want {
		t.Fatalf("类别统计\n得到 %s\n应为 %s", got, want)
	}
	// 默认范围（失败和已加密）的扩展名统计：不含仅文件名的
	if len(exts) == 0 || exts[0] != (ExtCount{"doc", 2}) && exts[0] != (ExtCount{"pdf", 2}) && exts[0] != (ExtCount{"xls", 2}) {
		t.Fatalf("扩展名统计: %v", exts)
	}
	for _, e := range exts {
		if e.Ext == "exe" || e.Ext == "mp4" {
			t.Fatalf("默认范围不应包含仅文件名的扩展名: %v", exts)
		}
	}
	// 选了扩展名：类别数只算该扩展名的
	cats, _, _ = db.ProblemSummary(ProblemFilter{Ext: "pdf"})
	if got := catCounts(cats); got != "network=1 corrupt=1" {
		t.Fatalf("pdf 的类别统计: %s", got)
	}
	// 选了「仅文件名」：没有扩展名的记为 "."
	_, exts, _ = db.ProblemSummary(ProblemFilter{Cat: "meta"})
	if fmt.Sprint(exts) != "[{jpg 2} {. 1} {exe 1} {mp4 1} {png 1}]" {
		t.Fatalf("仅文件名的扩展名统计: %v", exts)
	}
	// 路径关键字：盘符、反斜杠都可以
	cats, _, _ = db.ProblemSummary(ProblemFilter{Q: `F:\工作\`})
	if got := catCounts(cats); got != "encrypted=2 timeout=1 access=1 convert=1 corrupt=1" {
		t.Fatalf("按路径筛选: %s", got)
	}
	// 压缩包里的条目按条目路径也能搜到
	cats, _, _ = db.ProblemSummary(ProblemFilter{Q: "里面"})
	if got := catCounts(cats); got != "corrupt=1" {
		t.Fatalf("按压缩包内路径筛选: %s", got)
	}
}

// 分批加载：每批按编号顺序，下一批从上一批最后一个之后开始；查询走部分索引。
func TestListProblemsKeyset(t *testing.T) {
	db := problemDB(t)
	var all []Problem
	var after int64
	for {
		b, err := db.ListProblems(ProblemFilter{}, after, 4)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
		if len(b) < 4 {
			break
		}
		after = b[len(b)-1].ID
	}
	if len(all) != 10 {
		t.Fatalf("应有 10 个失败和已加密，得到 %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatal("没有按编号顺序")
		}
	}
	got, _ := db.ListProblems(ProblemFilter{Cat: "corrupt", Ext: "doc"}, 0, 100)
	if len(got) != 1 || got[0].Inner != "里面/坏.doc" || got[0].Cat != "corrupt" || ProblemLabel(got[0].Cat) != "文件损坏或格式不对" {
		t.Fatalf("按类别和扩展名筛选: %+v", got)
	}
	got, _ = db.ListProblems(ProblemFilter{Cat: "meta", Ext: "."}, 0, 100)
	if len(got) != 1 || got[0].RelPath != "个人/noext" || ProblemLabel(got[0].Cat) != "仅文件名：不支持的格式" {
		t.Fatalf("没有扩展名的: %+v", got)
	}
	// 关键字里的 % _ 按字面匹配
	if got, _ := db.ListProblems(ProblemFilter{Q: "%"}, 0, 100); len(got) != 0 {
		t.Fatalf("%% 应按字面匹配: %d", len(got))
	}
	rows, err := db.R.Query(`EXPLAIN QUERY PLAN SELECT id FROM files INDEXED BY files_problem WHERE state>=2 AND state<4 AND id>? ORDER BY id LIMIT 10`, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, " | "); !strings.Contains(p, "files_problem") || strings.Contains(p, "TEMP B-TREE") {
		t.Fatalf("查询计划: %s", p)
	}
}

// 整类重试：只重试失败和已加密的；压缩包里的条目重新排队它所在的压缩包。
func TestRetryProblems(t *testing.T) {
	db := problemDB(t)
	n, err := db.RetryProblems(ProblemFilter{Cat: "corrupt"})
	if err != nil || n != 2 {
		t.Fatalf("重试损坏类：%d %v（应为 d.pdf 和 包.zip）", n, err)
	}
	state := func(rel, inner string) int {
		var s int
		if err := db.R.QueryRow(`SELECT state FROM files WHERE rel_path=? AND inner=?`, rel, inner).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if state("工作/d.pdf", "") != StatePending || state("包.zip", "") != StatePending {
		t.Fatal("应重新排队")
	}
	if state("包.zip", "里面/坏.doc") != StateFailed {
		t.Fatal("压缩包里的条目不应单独排队（随压缩包重新提取）")
	}
	if n, _ := db.RetryProblems(ProblemFilter{Cat: "meta"}); n != 0 {
		t.Fatalf("仅文件名的不应重试: %d", n)
	}
	if n, _ := db.RetryProblems(ProblemFilter{Q: "个人"}); n != 3 {
		t.Fatalf("按路径重试: %d", n)
	}
	// 按编号重试：已加密的也可以
	var id int64
	db.R.QueryRow(`SELECT id FROM files WHERE rel_path='工作/a.xls'`).Scan(&id)
	if err := db.RetryFailed([]int64{id}); err != nil || state("工作/a.xls", "") != StatePending {
		t.Fatalf("按编号重试: %v", err)
	}
	// 全部重试：不含已加密
	if err := db.RetryFailed(nil); err != nil || state("工作/b.xls", "") != StateEncrypted || state("工作/c.doc", "") != StatePending {
		t.Fatalf("全部重试: %v", err)
	}
}

// 按索引根、文件夹筛选；统计各索引根的数量、某个文件夹下一级子文件夹的数量（中文文件夹名按字符切分）。
func TestProblemRootsAndFolders(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	add := func(root int64, rel string) {
		if _, err := d.W.Exec(`INSERT INTO files(root_id,volume_id,rel_path,name,ext,state,err) VALUES(?,'v',?,'n','pdf',2,'文件已损坏')`, root, rel); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"资料/合同/a.pdf", "资料/合同/2024/b.pdf", "资料/发票/c.pdf", "资料/d.pdf", "其他/e.pdf"} {
		add(1, rel)
	}
	add(2, "网盘/f.pdf")
	rc, err := d.ProblemRoots(ProblemFilter{})
	if err != nil || len(rc) != 2 || rc[0].Count+rc[1].Count != 6 {
		t.Fatalf("%+v %v", rc, err)
	}
	fs, here, err := d.ProblemFolders(ProblemFilter{Root: 1})
	if err != nil || here != 0 || len(fs) != 2 || fs[0] != (FolderCount{"资料", 4}) || fs[1] != (FolderCount{"其他", 1}) {
		t.Fatalf("最上层：%+v %d %v", fs, here, err)
	}
	fs, here, _ = d.ProblemFolders(ProblemFilter{Root: 1, Dir: "资料"})
	if here != 1 || len(fs) != 2 || fs[0] != (FolderCount{"合同", 2}) || fs[1] != (FolderCount{"发票", 1}) {
		t.Fatalf("资料：%+v %d", fs, here)
	}
	ps, _ := d.ListProblems(ProblemFilter{Root: 1, Dir: "资料/合同"}, 0, 100)
	if len(ps) != 2 {
		t.Fatalf("资料/合同 里应有 2 个（含子文件夹）：%d", len(ps))
	}
	if n, _ := d.RetryProblems(ProblemFilter{Root: 1, Dir: "资料/发票"}); n != 1 {
		t.Fatalf("重试这个文件夹：%d", n)
	}
	if _, _, err := d.ProblemFolders(ProblemFilter{}); err == nil {
		t.Fatal("没选索引根应报错")
	}
}
