package store

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func zlibBytes(s string) []byte {
	var b bytes.Buffer
	w, _ := zlib.NewWriterLevel(&b, zlib.BestSpeed)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func sampleText(i int) string {
	return fmt.Sprintf("第%d份合同：甲方应当在收到发票后三十日内支付合同价款，乙方保证所交付的货物符合国家标准。违约金为合同总额的百分之五。编号HT-%05d。", i, i) +
		strings.Repeat("双方应当遵守本合同约定，履行各自义务。", i%7+1)
}

// makeV1 用 0.1.x 的表结构建一个索引库：n 个文件，其中每 3 个内容相同（各存一份 zlib 文本）。
func makeV1(t *testing.T, dir string, n int) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "index.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	db, _ := conn.Begin()
	defer db.Commit()
	db.Exec(`INSERT INTO meta(k,v) VALUES('schema_version','1')`)
	db.Exec(`INSERT INTO volumes(id) VALUES('v1')`)
	db.Exec(`INSERT INTO roots(id,volume_id,rel_path,added_at) VALUES(1,'v1','',0)`)
	for i := 1; i <= n; i++ {
		content := i / 3 // 每 3 个文件内容相同
		text := sampleText(content)
		if _, err := db.Exec(`INSERT INTO files(id,root_id,volume_id,rel_path,name,ext,size,mtime,state,hash,text_len) VALUES(?,1,'v1',?,?,'txt',?,0,1,?,?)`,
			i, fmt.Sprintf("d/f%d.txt", i), fmt.Sprintf("f%d.txt", i), len(text), fmt.Sprintf("h%d", content), len([]rune(text))); err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO texts(file_id,flags,data,locs) VALUES(?,1,?,NULL)`, i, zlibBytes(text))
		db.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,?)`, i, "x", "f")
	}
	// 0.1.x 删除文件时偶尔留下的孤立文本
	db.Exec(`INSERT INTO texts(file_id,flags,data) VALUES(99999,1,?)`, zlibBytes("孤立"))
}

func countTexts(t *testing.T, db *DB) int {
	var n int
	db.R.QueryRow(`SELECT count(*) FROM texts`).Scan(&n)
	return n
}

func TestUpgradeFromV1AndMigrate(t *testing.T) {
	dir := t.TempDir()
	const n = 1200
	makeV1(t, dir, n)
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, _ := db.GetMeta("schema_version"); v != "2" {
		t.Errorf("schema_version %q", v)
	}
	if got := countTexts(t, db); got != n {
		t.Fatalf("升级后应有 %d 份文本（孤立的已删除），实际 %d", n, got)
	}
	check := func(stage string) {
		t.Helper()
		for _, i := range []int{1, 2, 3, 299, 600, 1200} {
			tx, err := db.GetText(int64(i))
			if err != nil || tx == nil || tx.Text != sampleText(i/3) || !tx.LineBased {
				t.Fatalf("%s：文件 %d 的文本不对：%v %+v", stage, i, err, tx)
			}
		}
	}
	check("升级后（旧的 zlib 数据）")

	// 合并重复文本
	total := 0
	for {
		g, st, err := db.DedupeBatch(50)
		if err != nil {
			t.Fatal(err)
		}
		total += st.Deduped
		if g == 0 {
			break
		}
	}
	if got := countTexts(t, db); got != n/3+1 || total != n-(n/3+1) {
		t.Errorf("合并后文本 %d 份（应为 %d），改为共用的文件 %d 个", got, n/3+1, total)
	}
	check("合并重复文本后")

	// 训练词典并重新压缩
	ok, err := db.TrainDict()
	if err != nil || !ok {
		t.Fatalf("训练词典：%v %v", ok, err)
	}
	var before, after int64
	db.R.QueryRow(`SELECT sum(length(data)) FROM texts`).Scan(&before)
	var last int64
	recompressed := 0
	for {
		var st MigrateStats
		last, st, err = db.RecompressBatch(last, 100)
		if err != nil {
			t.Fatal(err)
		}
		recompressed += st.Recompressed
		if last == 0 {
			break
		}
	}
	db.R.QueryRow(`SELECT sum(length(data)) FROM texts`).Scan(&after)
	t.Logf("重新压缩 %d 份：%d → %d 字节（%.0f%%）", recompressed, before, after, 100*float64(after)/float64(before))
	if recompressed != n/3+1 || after >= before {
		t.Errorf("重新压缩 %d 份，大小 %d → %d", recompressed, before, after)
	}
	check("重新压缩后")
	db.Close()

	// 重新打开：词典从索引库读出，数据照样能解开
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // Windows 上打开着的文件删不掉：临时目录清理前必须关闭
	if !db.HasDict() {
		t.Error("重新打开后应读到词典")
	}
	check("重新打开后")
}

// 共用的文本在最后一个引用它的文件删除或重新提取后才删除。
func TestSharedTextGC(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.UpsertVolume(Volume{ID: "v1"})
	rid, _ := db.AddRoot("v1", "")
	var ids []int64
	err = db.Tx(func(tx *sql.Tx) error {
		for i := 0; i < 3; i++ {
			f := &File{RootID: rid, VolumeID: "v1", RelPath: fmt.Sprintf("f%d.txt", i), Name: "f.txt", Ext: "txt", Size: 10, Hash: "same"}
			if err := UpsertFile(tx, f, "f"); err != nil {
				return err
			}
			ids = append(ids, f.ID)
		}
		if err := db.PutResult(tx, ids[0], StateDone, "", "same", false, &Text{Text: "共用的内容"}, "x", "f"); err != nil {
			return err
		}
		for _, id := range ids[1:] {
			if err := CopyResult(tx, ids[0], id, "x", "f"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if countTexts(t, db) != 1 {
		t.Fatalf("内容相同的 3 个文件应只存 1 份文本，实际 %d", countTexts(t, db))
	}
	if dup, _ := db.FindDuplicate("same", 10, ids[2]); dup == 0 {
		t.Error("应找到内容相同的文件")
	}
	// 删除最早提取的那个：其余两个仍能读到
	if err := db.DeleteFiles(ids[:1]); err != nil {
		t.Fatal(err)
	}
	if tx, _ := db.GetText(ids[1]); tx == nil || tx.Text != "共用的内容" {
		t.Fatalf("删除一个文件后，共用文本应保留：%+v", tx)
	}
	// 另一个重新提取成不同内容：旧文本还被第三个引用
	db.Tx(func(tx *sql.Tx) error {
		return db.PutResult(tx, ids[1], StateDone, "", "other", false, &Text{Text: "新内容"}, "x", "f")
	})
	if countTexts(t, db) != 2 {
		t.Errorf("应有 2 份文本，实际 %d", countTexts(t, db))
	}
	// 最后一个引用变化（扫描发现文件被修改）：旧文本被删除
	db.Tx(func(tx *sql.Tx) error {
		return UpsertFile(tx, &File{RootID: rid, VolumeID: "v1", RelPath: "f2.txt", Name: "f.txt", Ext: "txt", Size: 11}, "f")
	})
	if countTexts(t, db) != 1 {
		t.Errorf("没有文件引用的文本应自动删除，剩 %d 份", countTexts(t, db))
	}
	if err := db.RemoveRoot(rid); err != nil {
		t.Fatal(err)
	}
	if countTexts(t, db) != 0 {
		t.Errorf("删除索引根后应没有文本，剩 %d 份", countTexts(t, db))
	}
}

// 删除的文件的文本先进回收区：同样内容的文件出现时（移动、改名）直接复用；过期后删除。
// 移除整个索引根时不进回收区。
func TestRecycleOnDelete(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.UpsertVolume(Volume{ID: "v1"})
	rid, _ := db.AddRoot("v1", "")
	add := func(rel, hash string, text string) int64 {
		f := &File{RootID: rid, VolumeID: "v1", RelPath: rel, Name: rel, Ext: "pdf", Size: 100, Hash: hash}
		db.Tx(func(tx *sql.Tx) error {
			if err := UpsertFile(tx, f, "n"); err != nil {
				return err
			}
			if text == "" {
				return nil
			}
			return db.PutResult(tx, f.ID, StateDone, "", hash, true, &Text{Text: text}, "x", "n")
		})
		return f.ID
	}
	count := func(table string) int {
		var n int
		db.R.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n)
		return n
	}
	a := add("旧位置/扫描件.pdf", "h1", "OCR 识别出的内容")
	if err := db.DeleteFiles([]int64{a}); err != nil {
		t.Fatal(err)
	}
	if count("recycled") != 1 || count("texts") != 1 {
		t.Fatalf("删除后文本应进回收区：recycled %d texts %d", count("recycled"), count("texts"))
	}
	r, err := db.FindRecycled("h1", 100)
	if err != nil || r == nil || !r.OCR {
		t.Fatalf("应在回收区找到：%+v %v", r, err)
	}
	b := add("新位置/扫描件.pdf", "", "")
	if err := db.Tx(func(tx *sql.Tx) error { return AttachText(tx, b, r, "h1", "x", "n") }); err != nil {
		t.Fatal(err)
	}
	if tx, _ := db.GetText(b); tx == nil || tx.Text != "OCR 识别出的内容" || count("recycled") != 0 {
		t.Fatalf("复用后：%+v recycled %d", tx, count("recycled"))
	}
	if f, _ := db.GetFile(b); f.State != StateDone || !f.OCR || f.Hash != "h1" {
		t.Errorf("复用后的文件信息：%+v", f)
	}
	db.DeleteFiles([]int64{b})
	if n, _ := db.PurgeRecycled(time.Now().Add(time.Hour)); n != 1 || count("texts") != 0 {
		t.Errorf("过期后应删除：%d texts %d", n, count("texts"))
	}
	add("c.pdf", "h2", "内容")
	if err := db.RemoveRoot(rid); err != nil {
		t.Fatal(err)
	}
	if count("recycled") != 0 || count("texts") != 0 {
		t.Errorf("移除索引根不进回收区：recycled %d texts %d", count("recycled"), count("texts"))
	}
}
