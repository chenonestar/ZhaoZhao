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
