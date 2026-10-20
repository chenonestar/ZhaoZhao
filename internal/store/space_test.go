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

// fillTexts 插入 n 个文件，每两个文件共用一份文本。
func fillTexts(t *testing.T, db *DB, n int) {
	t.Helper()
	tx, err := db.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if i%2 == 1 {
			if _, err := tx.Exec(`INSERT INTO texts(id,flags,data) VALUES(?,2,x'00')`, i); err != nil {
				t.Fatal(err)
			}
		}
		tid := i - (i+1)%2
		if _, err := tx.Exec(`INSERT INTO files(id,volume_id,rel_path,root_id,name,ext,state,text_id) VALUES(?,'v',?,1,'n','txt',1,?)`, i, fmt.Sprint(i), tid); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// 删除文件、重新提取（换文本）时，触发器按文本找文件要用上索引：每个文件的耗时不能随文件总数增长。
// 0.2.3 之前每次都扫整张文件表，14 万个文件时每个文件多花约 11 毫秒，删除一个大的索引根要半小时。
// 空间统计同理（以前 10 万份文本要几十分钟）。
func TestTextLookupsUseIndex(t *testing.T) {
	cost := func(n int) (time.Duration, time.Duration) {
		db, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		fillTexts(t, db, n)
		t0 := time.Now()
		if _, err := db.W.Exec(`UPDATE files SET text_id=0 WHERE id<=400`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.W.Exec(`DELETE FROM files WHERE id>400 AND id<=800`); err != nil {
			t.Fatal(err)
		}
		d := time.Since(t0)
		var texts int
		db.R.QueryRow(`SELECT count(*) FROM texts`).Scan(&texts)
		if texts != n/2-400 {
			t.Fatalf("没有文件引用的文本应删除：剩 %d 份", texts)
		}
		t0 = time.Now()
		st, err := db.Stats()
		if err != nil || st.Texts == 0 || len(st.ByExt) != 1 {
			t.Fatalf("%+v %v", st, err)
		}
		return d, time.Since(t0)
	}
	small, smallStats := cost(2000)
	big, bigStats := cost(40000)
	t.Logf("2000 个文件：改/删 800 个 %v，统计 %v；40000 个文件：%v，%v", small, smallStats, big, bigStats)
	if big > 4*small+200*time.Millisecond {
		t.Fatalf("文件多了 20 倍，改/删同样多的文件慢了 %.1f 倍：按文本找文件没有用上索引", float64(big)/float64(small))
	}
	if bigStats > 40*smallStats+2*time.Second {
		t.Fatalf("文件多了 20 倍，空间统计慢了 %.1f 倍：按文本找文件没有用上索引", float64(bigStats)/float64(smallStats))
	}
}

// 旧索引库里的触发器（按文本找文件不带 text_id>0）打开时换成新的。
func TestOldTriggersReplaced(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.Exec(`CREATE TRIGGER files_text_gone AFTER DELETE ON files WHEN OLD.text_id>0 BEGIN
  DELETE FROM texts WHERE id=OLD.text_id AND NOT EXISTS (SELECT 1 FROM files WHERE text_id=OLD.text_id);
END`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.R.Query(`SELECT name FROM sqlite_master WHERE type='trigger' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if fmt.Sprint(names) != "[files_text_changed2 files_text_gone2 recycled_gone2]" {
		t.Fatalf("触发器：%v", names)
	}
}
