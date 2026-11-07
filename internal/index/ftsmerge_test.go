package index

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/store"
)

// 空闲时把全文索引合并成一个段；做完记下时间，一天之内不再做；不值得做时不做。
func TestMergeFTSWhenIdle(t *testing.T) {
	data := t.TempDir()
	db, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := New(db, &fakeProv{}, nil, data)
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	m.sched.idle.Store(true)

	// 太小：不整理
	if _, err := db.W.Exec(`INSERT INTO fts(rowid,c,n) VALUES(1,'a b c','x')`); err != nil {
		t.Fatal(err)
	}
	m.mergeFTS()
	if v, _ := db.GetMeta("fts_merged_at"); v != "" {
		t.Fatal("全文索引很小时不必整理")
	}

	// 写入很多批（每批一个段），再删掉一部分
	id := 1
	for batch := 0; batch < 60; batch++ {
		tx, err := db.W.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for k := 0; k < 40; k++ {
			id++
			var b strings.Builder
			for j := 0; j < 400; j++ {
				fmt.Fprintf(&b, "w%d ", (id*7+j*13)%2000)
			}
			if _, err := tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,'name')`, id, b.String()); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.W.Exec(`DELETE FROM fts WHERE rowid % 16 = 0`); err != nil {
		t.Fatal(err)
	}
	old := ftsMergeMin
	ftsMergeMin = 100
	defer func() { ftsMergeMin = old }()
	before, err := db.FTSInfo()
	if err != nil {
		t.Fatal(err)
	}
	if !ftsNeedsMerge(before) {
		t.Fatalf("测试数据应当需要整理: %+v", before)
	}
	m.mergeFTS()
	after, err := db.FTSInfo()
	if err != nil {
		t.Fatal(err)
	}
	if after.Segments != 1 || after.Deleted != 0 {
		t.Fatalf("空闲时应当合并成一个段: 合并前 %+v，合并后 %+v", before, after)
	}
	if v, _ := db.GetMeta("fts_merged_at"); v == "" {
		t.Fatal("做完应当记下时间")
	}
	if v, _ := db.GetMeta("fts_merge"); v != "" {
		t.Fatalf("做完应当清掉进行中的记录: %q", v)
	}

	// 一天之内再有新的段也不整理
	if _, err := db.W.Exec(`DELETE FROM fts WHERE rowid % 5 = 0`); err != nil {
		t.Fatal(err)
	}
	mid, _ := db.FTSInfo()
	m.mergeFTS()
	if again, _ := db.FTSInfo(); again != mid {
		t.Fatalf("一天之内不应再整理: %+v → %+v", mid, again)
	}

	// 索引忙时不动；进行中的整理（上次中途停下）空闲后接着做完
	_ = db.SetMeta("fts_merge", "0 1")
	m.sched.idle.Store(false)
	m.mergeFTS()
	if v, _ := db.GetMeta("fts_merge"); v == "" {
		t.Fatal("索引忙时不应结束整理")
	}
	m.sched.idle.Store(true)
	m.mergeFTS()
	if v, _ := db.GetMeta("fts_merge"); v != "" {
		t.Fatal("空闲后应当接着做完")
	}
}

func TestFTSNeedsMerge(t *testing.T) {
	for _, c := range []struct {
		fi   store.FTSInfo
		want bool
	}{
		{store.FTSInfo{Segments: 5, Pages: 1000, Largest: 100}, false},                     // 太小
		{store.FTSInfo{Segments: 1, Pages: 100000, Largest: 100000, Entries: 1000}, false}, // 已经是一个段
		{store.FTSInfo{Segments: 3, Pages: 100000, Largest: 95000, Entries: 1000}, false},  // 新数据不到一成
		{store.FTSInfo{Segments: 3, Pages: 100000, Largest: 85000, Entries: 1000}, true},
		{store.FTSInfo{Segments: 1, Pages: 100000, Largest: 100000, Entries: 1000, Deleted: 60}, true}, // 残留条目多
	} {
		if got := ftsNeedsMerge(c.fi); got != c.want {
			t.Errorf("%+v: 得到 %v，应当是 %v", c.fi, got, c.want)
		}
	}
}
