package index

import (
	"path/filepath"
	"strings"
	"testing"
)

// 排除所在文件夹：加入「排除的路径前缀」，文件夹里的文件立即移出索引，重新扫描也不再进去；
// 别的文件夹不受影响；索引根最上层的文件不能这样排除（会排除整个索引根）。
func TestExcludeFolders(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "资料", "a.txt"), "保留 aardvark")
	write(t, filepath.Join(dir, "资料", "安装包", "b.txt"), "排除 bonobo")
	write(t, filepath.Join(dir, "资料", "安装包", "更深", "c.txt"), "排除 civet")
	write(t, filepath.Join(dir, "top.txt"), "最上层")
	prov := &fakeProv{}
	prov.add("v1", "数据", dir, true)
	e := newEnv(t, prov)
	if _, err := e.m.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	e.waitIdle()
	id := func(name string) int64 {
		var id int64
		e.m.DB.R.QueryRow(`SELECT id FROM files WHERE name=?`, name).Scan(&id)
		return id
	}
	if _, err := e.m.ExcludeFolders([]int64{id("top.txt")}); err == nil || !strings.Contains(err.Error(), "不能排除整个索引根") {
		t.Fatalf("最上层的文件：%v", err)
	}
	got, err := e.m.ExcludeFolders([]int64{id("b.txt"), id("c.txt")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "安装包") {
		t.Fatalf("「更深」在「安装包」里，只排除「安装包」：%v", got)
	}
	if id("b.txt") != 0 || id("c.txt") != 0 || id("a.txt") == 0 {
		t.Fatal("文件夹里的文件应立即移出索引，别的不动")
	}
	if p := e.m.Config().ExcludePrefixes; !strings.HasSuffix(p[len(p)-1], "安装包") {
		t.Fatalf("排除规则：%v", p)
	}
	e.m.RescanAll()
	e.waitIdle()
	if id("b.txt") != 0 || id("c.txt") != 0 || id("a.txt") == 0 {
		t.Fatal("重新扫描后也不应再进来")
	}
	// 再排除一次同一个文件夹：规则不重复
	n := len(e.m.Config().ExcludePrefixes)
	if _, err := e.m.ExcludeFolders([]int64{id("a.txt")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.ExcludeFolders([]int64{id("a.txt")}); err == nil {
		t.Fatal("文件已移出索引，再排除应提示没有可以排除的")
	}
	if len(e.m.Config().ExcludePrefixes) != n+1 {
		t.Fatal("规则不应重复")
	}
}
