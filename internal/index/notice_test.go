package index

import (
	"testing"
)

func volumeNotices(m *Manager) []Notice {
	var out []Notice
	for _, n := range m.Notices(0) {
		if n.Kind == "new-volume" {
			out = append(out, n)
		}
	}
	return out
}

// 新接入的磁盘问一次要不要加入索引：拔出后提示消失、再插上再问；点「忽略」后记住这块盘，以后不再问；
// 加入索引后提示消失。以前「忽略」只在界面上隐藏，重新打开窗口、拔出磁盘后提示都还在。
func TestNewVolumeNotice(t *testing.T) {
	prov := &fakeProv{}
	prov.add("v1", "数据", t.TempDir(), true)
	e := newEnv(t, prov)
	m := e.m
	if len(volumeNotices(m)) != 0 {
		t.Fatal("启动时已有的磁盘不应提示")
	}
	u1, u2 := t.TempDir(), t.TempDir()
	prov.add("u1", "U盘", u1, true)
	m.RefreshVolumes()
	ns := volumeNotices(m)
	if len(ns) != 1 || ns[0].Volume != "u1" {
		t.Fatalf("插上新磁盘应提示一次：%+v", ns)
	}
	m.RefreshVolumes()
	if len(volumeNotices(m)) != 1 {
		t.Fatal("同一块盘不应重复提示")
	}
	prov.set("u1", false, "")
	m.RefreshVolumes()
	if len(volumeNotices(m)) != 0 {
		t.Fatal("拔出后提示应消失")
	}
	prov.set("u1", true, "")
	m.RefreshVolumes()
	ns = volumeNotices(m)
	if len(ns) != 1 {
		t.Fatal("再插上应再问")
	}
	m.DismissNotice(ns[0].ID)
	if len(volumeNotices(m)) != 0 {
		t.Fatal("忽略后提示应消失")
	}
	prov.set("u1", false, "")
	m.RefreshVolumes()
	prov.set("u1", true, "")
	m.RefreshVolumes()
	if len(volumeNotices(m)) != 0 {
		t.Fatal("忽略过的磁盘再插上不应再问")
	}
	// 另一块盘：加入索引后提示消失
	prov.add("u2", "移动硬盘", u2, true)
	m.RefreshVolumes()
	if len(volumeNotices(m)) != 1 {
		t.Fatal("另一块新磁盘应提示")
	}
	if _, err := m.AddRoot(u2); err != nil {
		t.Fatal(err)
	}
	if len(volumeNotices(m)) != 0 {
		t.Fatal("加入索引后提示应消失")
	}
	// 忽略的磁盘记在索引库里：重启后也不问
	if ids := m.ignoredVolumes(); len(ids) != 1 || ids[0] != "u1" {
		t.Fatalf("应记住忽略的磁盘：%v", ids)
	}
}
