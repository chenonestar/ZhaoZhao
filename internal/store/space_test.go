package store

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// WAL 超过上限时主动写回并截短；有读者没读完时不硬等，返回 busy，读者读完后再做就能截短。
func TestCheckpointIfLarge(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	walSize := func() int64 {
		st, err := os.Stat(db.Path + "-wal")
		if err != nil {
			return 0
		}
		return st.Size()
	}
	for i := range 50 {
		if err := db.SetMeta(fmt.Sprint("k", i), "v"); err != nil {
			t.Fatal(err)
		}
	}
	if did, _, err := db.CheckpointIfLarge(1<<30, time.Second); did || err != nil {
		t.Fatalf("WAL 没超过上限也做了检查点: %v %v", did, err)
	}
	// 读者从 WAL 里读的快照还没结束：截短不了
	rtx, err := db.R.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rtx.QueryRow(`SELECT count(*) FROM meta`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("after", "v"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	did, busy, err := db.CheckpointIfLarge(0, 200*time.Millisecond)
	if err != nil || !did || !busy {
		t.Fatalf("有读者时应当返回 busy: did=%v busy=%v err=%v", did, busy, err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("等读者等了 %v，超过给定的时限", d)
	}
	if walSize() == 0 {
		t.Fatal("有读者时 WAL 不应被截短")
	}
	rtx.Rollback()
	did, busy, err = db.CheckpointIfLarge(0, time.Second)
	if err != nil || !did || busy {
		t.Fatalf("读者读完后应当能写回: did=%v busy=%v err=%v", did, busy, err)
	}
	if s := walSize(); s != 0 {
		t.Fatalf("WAL 没有截短：%d 字节", s)
	}
	// 写入连接的等待时间恢复原样
	var ms int
	if err := db.W.QueryRow(`PRAGMA busy_timeout`).Scan(&ms); err != nil || ms != int(busyTimeout.Milliseconds()) {
		t.Fatalf("busy_timeout = %d（%v），应恢复为 %d", ms, err, busyTimeout.Milliseconds())
	}
}
