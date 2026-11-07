package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/store"
)

func TestOpenDBRecoversFromCorruption(t *testing.T) {
	dir := t.TempDir()
	// 先建一个有索引根的正常库，并写出备份
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db.UpsertVolume(store.Volume{ID: "vol:1", Label: "备份盘", Kind: "removable", LastMount: `E:\`})
	db.AddRoot("vol:1", "归档")
	rs, _ := db.ListRoots()
	if len(rs) != 1 {
		t.Fatal(rs)
	}
	backupRoots(t, db, dir)
	db.Close()
	// 破坏索引库
	if err := os.WriteFile(filepath.Join(dir, "index.db"), []byte("this is not a sqlite database at all, just garbage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "index.db-wal"))
	os.Remove(filepath.Join(dir, "index.db-shm"))

	db2, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := db2.Check(); err != nil {
		t.Fatal(err)
	}
	rs, _ = db2.ListRoots()
	if len(rs) != 1 || rs[0].RelPath != "归档" {
		t.Fatalf("索引根应从备份恢复: %+v", rs)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "index.db.corrupt-*"))
	if len(m) == 0 {
		t.Fatal("损坏的旧库应改名保留")
	}
}

func backupRoots(t *testing.T, db *store.DB, dir string) {
	t.Helper()
	// 与 index.Manager.saveRootsBackup 同样的格式
	vs, _ := db.ListVolumes()
	rs, _ := db.ListRoots()
	var b []byte
	b = append(b, []byte(`{"roots":[{"volume":{"id":"`+vs[0].ID+`","label":"`+vs[0].Label+`","serial":"","guid":"","capacity":0,"kind":"removable","lastMount":"E:\\","lastSeen":0},"rel":"`+rs[0].RelPath+`","opts":{}}]}`)...)
	if err := os.WriteFile(filepath.Join(dir, "roots.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}
