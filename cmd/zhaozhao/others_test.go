package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err := adoptDataDir(k.DataDir, cur, ""); err != nil {
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
	// 安装版的数据目录里只有用过的目录列表时照样能搬，列表留着
	inst := filepath.Join(base, "appdata", "ZhaoZhao")
	if err := adoptDataDir(cur, inst, ""); err != nil || !hasIndex(inst) {
		t.Fatalf("搬到安装版的数据目录：%v", err)
	}
	if k := loadKnown(); len(k) != 3 {
		t.Fatalf("用过的目录列表应留着：%+v", k)
	}
	cur = inst
	// 新目录不是空的时候不搬
	other := filepath.Join(base, "x", "data")
	os.MkdirAll(other, 0o700)
	os.WriteFile(filepath.Join(other, "index.db"), []byte("x"), 0o600)
	if err := adoptDataDir(other, cur, ""); err == nil {
		t.Fatal("目标不是空目录时应报错")
	}
}

// 这里已有索引、但比旧索引小得多（刚开始建）时也问要不要换；换时这里的索引改名留着；选了不换以后不再问。
func TestAdoptOverSmallIndex(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(base, "appdata"))
	old := filepath.Join(base, "找找0.2.13", "data")
	cur := filepath.Join(base, "找找0.2.14", "data")
	for _, d := range []string{old, cur} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(old, "index.db"), []byte(strings.Repeat("x", 5000)), 0o600)
	os.WriteFile(filepath.Join(old, declinedFile), nil, 0o600) // 旧目录以前记下的，搬过来后不算
	os.WriteFile(filepath.Join(cur, "index.db"), []byte(strings.Repeat("y", 400)), 0o600)
	os.WriteFile(filepath.Join(cur, "index.db-wal"), []byte(strings.Repeat("y", 400)), 0o600)
	rememberDataDir(old, "a.exe")
	if _, ok := adoptCandidate(cur, nil); ok {
		t.Fatal("旧索引不到这里的 10 倍时不问")
	}
	os.WriteFile(filepath.Join(cur, "index.db-wal"), nil, 0o600)
	if k, ok := adoptCandidate(cur, nil); !ok || !samePath(k.DataDir, old) {
		t.Fatal("旧索引比这里大得多时应找到")
	}

	var asked []string
	answer := 1
	askFunc = func(title, main, content string, buttons []string) int {
		asked = append(asked, main+"｜"+content)
		return answer
	}
	t.Cleanup(func() { askFunc = askChoice })
	startNotes, askWait = nil, 0
	if !checkOthers(cur) || len(asked) != 1 || !strings.Contains(asked[0], "才建了一点") {
		t.Fatalf("应问一次：%q", asked)
	}
	if _, err := os.Stat(filepath.Join(cur, declinedFile)); err != nil {
		t.Fatal("选了不换应记下")
	}
	checkOthers(cur)
	if len(asked) != 1 {
		t.Fatal("选过不换后不再问")
	}

	// 换成旧索引：这里的改名留着
	os.Remove(filepath.Join(cur, declinedFile))
	answer, asked = 0, nil
	if !checkOthers(cur) || len(asked) != 1 {
		t.Fatalf("应问一次：%q", asked)
	}
	if indexSize(cur) != 5000 || hasIndex(old) {
		t.Fatalf("应换成旧索引：%d", indexSize(cur))
	}
	if _, err := os.Stat(filepath.Join(cur, declinedFile)); err == nil {
		t.Fatal("旧目录里的「不再问」不应带过来")
	}
	m, _ := filepath.Glob(cur + "-换下-*")
	if len(m) != 1 || indexSize(m[0]) != 400 {
		t.Fatalf("这里原来的索引应改名留着：%q；经过：%q", m, startNotes)
	}
	if askWait <= 0 {
		t.Fatal("应记下等用户选择的时间")
	}

	// 搬不过来时换回这里的索引
	x := filepath.Join(base, "x", "data")
	aside := asideName(x, time.Now())
	os.MkdirAll(x, 0o700)
	os.WriteFile(filepath.Join(x, "index.db"), []byte("z"), 0o600)
	if err := adoptDataDir(filepath.Join(base, "没有"), x, aside); err == nil || indexSize(x) != 1 {
		t.Fatalf("搬不过来时应报错并换回：%v", err)
	}

	// 安装版的数据目录：用过的目录列表留在原处，不跟着小索引改名
	inst := filepath.Join(base, "appdata", "ZhaoZhao")
	os.WriteFile(filepath.Join(inst, "index.db"), []byte("w"), 0o600)
	ia := asideName(inst, time.Now())
	if err := adoptDataDir(cur, inst, ia); err != nil || indexSize(inst) != 5000 {
		t.Fatalf("换安装版的索引：%v", err)
	}
	if len(loadKnown()) == 0 {
		t.Fatal("用过的目录列表应留着")
	}
	if _, err := os.Stat(filepath.Join(ia, "known-data.json")); err == nil {
		t.Fatal("用过的目录列表不应跟着改名")
	}
}
