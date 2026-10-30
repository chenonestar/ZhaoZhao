package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 新文件夹里没有索引时，找以前用过、同一个盘上、没有在用的数据目录，搬过来（改名）接着用。
func TestAdoptDataDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(base, "appdata"))
	old := filepath.Join(base, "找找0.2.10", "data")
	empty := filepath.Join(base, "找找0.2.9", "data")
	for _, d := range []string{old, empty} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(old, "index.db"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(old, "config.json"), []byte("{}"), 0o600)
	rememberDataDir(empty, "a.exe") // 没有索引的不算
	rememberDataDir(old, "b.exe")
	cur := filepath.Join(base, "找找0.2.11", "data")
	os.MkdirAll(cur, 0o700) // 启动时已经建好的空目录
	if hasIndex(cur) {
		t.Fatal("新目录不应有索引")
	}
	k, ok := adoptCandidate(cur, nil)
	if !ok || !samePath(k.DataDir, old) || k.Exe != "b.exe" {
		t.Fatalf("应找到有索引的旧目录：%+v %v", k, ok)
	}
	if err := adoptDataDir(k.DataDir, cur); err != nil {
		t.Fatal(err)
	}
	if !hasIndex(cur) || hasIndex(old) {
		t.Fatal("应搬到新目录")
	}
	if b, _ := os.ReadFile(filepath.Join(cur, "config.json")); string(b) != "{}" {
		t.Fatal("设置也应搬过来")
	}
	rememberDataDir(cur, "c.exe")
	if k := loadKnown(); len(k) != 3 || !samePath(k[0].DataDir, cur) {
		t.Fatalf("最近用的在前：%+v", k)
	}
	if _, ok := adoptCandidate(cur, nil); ok {
		t.Fatal("已经没有别的有索引的目录")
	}
	// 新目录不是空的时候不搬
	other := filepath.Join(base, "x", "data")
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(other, "index.db"), []byte("x"), 0o600)
	if err := adoptDataDir(other, cur); err == nil {
		t.Fatal("目标不是空目录时应报错")
	}
}
