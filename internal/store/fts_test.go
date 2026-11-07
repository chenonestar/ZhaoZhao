package store

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// 分步合并全文索引：段合成一个，已删除的条目清掉，搜索结果不变；每步只做一点，要好几步才做完。
func TestMergeFTS(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	words := make([]string, 300)
	for i := range words {
		words[i] = fmt.Sprintf("w%03d", i)
	}
	r := rand.New(rand.NewPCG(1, 2))
	id := 0
	// 每批一个事务：每个事务生成一个段
	for batch := 0; batch < 40; batch++ {
		tx, err := db.W.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 50; k++ {
			id++
			var b strings.Builder
			for j := 0; j < 200; j++ {
				b.WriteString(words[r.IntN(len(words))])
				b.WriteByte(' ')
			}
			if _, err := tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,'name')`, id, b.String()); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.W.Exec(`DELETE FROM fts WHERE rowid % 20 = 0`); err != nil {
		t.Fatal(err)
	}
	count := func(w string) int {
		var n int
		if err := db.R.QueryRow(`SELECT count(*) FROM fts WHERE fts MATCH ?`, w).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := map[string]int{}
	for _, w := range []string{"w000", "w123", "w299", "name"} {
		before[w] = count(w)
	}
	info, err := db.FTSInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Segments < 2 || info.Deleted == 0 || info.Pages == 0 {
		t.Fatalf("测试数据应当有多个段和已删除的条目: %+v", info)
	}
	steps := 0
	for start := true; ; start = false {
		more, err := db.MergeFTS(20, start)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		steps++
		if steps == 1 {
			if mid, err := db.FTSInfo(); err != nil || mid.Merging == 0 {
				t.Fatalf("合并进行中应当能读到剩余的页数: %+v %v", mid, err)
			}
		}
		if steps > 10000 {
			t.Fatal("合并做不完")
		}
	}
	if steps < 2 {
		t.Fatalf("每步只写 20 页，应当分好几步做完，实际 %d 步", steps)
	}
	after, err := db.FTSInfo()
	if err != nil {
		t.Fatal(err)
	}
	if after.Segments != 1 || after.Deleted != 0 || after.Merging != 0 || after.Pages >= info.Pages {
		t.Fatalf("合并后应当只剩一个更小的段: 合并前 %+v，合并后 %+v", info, after)
	}
	t.Logf("合并前 %+v，合并后 %+v，%d 步", info, after, steps)
	for w, n := range before {
		if got := count(w); got != n {
			t.Fatalf("合并后 %s 搜到 %d 个，合并前 %d 个", w, got, n)
		}
	}
	// 已经合并完：再开始一次什么也不做
	if more, err := db.MergeFTS(20, true); err != nil || more {
		t.Fatalf("只有一个段时不应再合并: %v %v", more, err)
	}
}

func TestParseFTSStructureRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{nil, {1, 2, 3}, {0, 0, 0, 1, 5}, {0, 0, 0, 1, 1, 2, 0, 0, 1}} {
		if _, err := parseFTSStructure(b); err == nil {
			t.Fatalf("% x 不是完整的结构记录，应当报错", b)
		}
	}
}

// 批量写入：全文索引不边写边合并，段多攒一些（同一层最多 16 个）；恢复后照常自动合并。搜索结果不受影响。
func TestFTSBulk(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := rand.New(rand.NewPCG(3, 4))
	id := 0
	// 每批一个事务、一个段；每批的数据够多，默认设置下会边写边合并
	write := func(batches int) {
		for b := 0; b < batches; b++ {
			tx, err := db.W.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for k := 0; k < 50; k++ {
				id++
				var sb strings.Builder
				sb.WriteString("共同 ")
				for j := 0; j < 200; j++ {
					fmt.Fprintf(&sb, "w%d ", r.IntN(3000))
				}
				if _, err := tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,'')`, id, sb.String()); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	segs := func() int {
		fi, err := db.FTSInfo()
		if err != nil {
			t.Fatal(err)
		}
		return fi.Segments
	}
	if on, err := db.FTSBulk(); err != nil || on {
		t.Fatalf("新索引库不是批量写入：%v %v", on, err)
	}
	if err := db.SetFTSBulk(true); err != nil {
		t.Fatal(err)
	}
	if on, _ := db.FTSBulk(); !on {
		t.Fatal("应处于批量写入")
	}
	write(12)
	bulk := segs()
	if bulk != 12 {
		t.Fatalf("批量写入：12 批应是 12 个段（不到 16 个不合并），实际 %d 个", bulk)
	}
	if err := db.SetFTSBulk(false); err != nil {
		t.Fatal(err)
	}
	if on, _ := db.FTSBulk(); on {
		t.Fatal("应已恢复")
	}
	write(12)
	if s := segs(); s >= bulk {
		t.Fatalf("恢复后应照常边写边合并：%d → %d 个段", bulk, s)
	}
	var n int
	if err := db.R.QueryRow(`SELECT count(*) FROM fts WHERE fts MATCH '共同'`).Scan(&n); err != nil || n != id {
		t.Fatalf("搜到 %d 个（%v），应为 %d", n, err, id)
	}
}
