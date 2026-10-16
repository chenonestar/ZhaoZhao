package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// 待处理队列的查询要走部分索引 files_queue：按修改时间倒序直接取，不对十几万行排序。
func TestNextPendingUsesQueueIndex(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.R.Query(`EXPLAIN QUERY PLAN SELECT `+fileCols+` FROM files WHERE state=0 AND prio IN (?) AND root_id NOT IN (SELECT id FROM roots WHERE paused=1) AND id NOT IN (?,?) ORDER BY prio, mtime DESC LIMIT ?`, 2, 1, 2, 8)
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
	p := strings.Join(plan, " | ")
	if !strings.Contains(p, "files_queue") || strings.Contains(p, "TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("查询计划没有用上 files_queue 或需要额外排序: %s", p)
	}
}

// 同一优先级里最近修改的文件先处理。
func TestNextPendingRecentFirst(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rid, _ := db.AddRoot("v", "")
	err = db.Tx(func(tx *sql.Tx) error {
		for i, mt := range []int64{100, 300, 200} {
			f := &File{RootID: rid, VolumeID: "v", RelPath: fmt.Sprintf("f%d.txt", i), Name: fmt.Sprintf("f%d.txt", i), MTime: mt, Prio: 1}
			if err := UpsertFile(tx, f, "f"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := db.NextPending(10, []int{1}, nil, nil, false)
	if err != nil || len(fs) != 3 || fs[0].MTime != 300 || fs[1].MTime != 200 || fs[2].MTime != 100 {
		t.Fatalf("%v %+v", err, fs)
	}
}

// OCR 队列小文件在前，并走部分索引 files_queue_size，不对排队的文件排序。
func TestNextPendingSmallFirst(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rid, _ := db.AddRoot("v", "")
	err = db.Tx(func(tx *sql.Tx) error {
		for i, sz := range []int64{5000, 100, 70000, 900} {
			f := &File{RootID: rid, VolumeID: "v", RelPath: fmt.Sprintf("s%d.pdf", i), Name: fmt.Sprintf("s%d.pdf", i), Size: sz, MTime: int64(i), Prio: 3}
			if err := UpsertFile(tx, f, "f"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := db.NextPending(10, []int{3}, nil, nil, true)
	if err != nil || len(fs) != 4 || fs[0].Size != 100 || fs[1].Size != 900 || fs[3].Size != 70000 {
		t.Fatalf("%v %+v", err, fs)
	}
	rows, err := db.R.Query(`EXPLAIN QUERY PLAN SELECT `+fileCols+` FROM files WHERE state=0 AND prio IN (?) AND root_id NOT IN (SELECT id FROM roots WHERE paused=1) ORDER BY prio, size LIMIT ?`, 3, 8)
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
	if p := strings.Join(plan, " | "); !strings.Contains(p, "files_queue_size") || strings.Contains(p, "TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("查询计划: %s", p)
	}
}
