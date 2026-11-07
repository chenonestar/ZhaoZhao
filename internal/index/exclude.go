package index

import (
	"errors"
	"fmt"
	"path"

	"github.com/chenonestar/zhaozhao/internal/logx"
)

// ExcludeFolders 把这些文件所在的文件夹加入排除规则（「排除的路径前缀」），并立即把文件夹里已索引的
// 文件移出索引（之后的重新扫描也不再进去）。压缩包里的条目按压缩包所在的文件夹算。返回排除的文件夹。
func (m *Manager) ExcludeFolders(ids []int64) ([]string, error) {
	roots, err := m.DB.ListRoots()
	if err != nil {
		return nil, err
	}
	type folder struct{ vol, rel, abs string }
	var fs []folder
	seen := map[string]bool{}
	for _, id := range ids {
		f, err := m.DB.GetFile(id)
		if err != nil || f == nil {
			continue
		}
		rel := path.Dir(f.RelPath)
		if rel == "." || rel == "/" {
			rel = ""
		}
		for _, r := range roots {
			if r.VolumeID == f.VolumeID && nestedIn(r.RelPath, rel) {
				return nil, fmt.Errorf("「%s」就在索引根「%s」的最上层：不能排除整个索引根，不需要的话请移除这个索引根",
					f.Name, m.rootDisplay(r))
			}
		}
		abs, online := m.AbsPath(f.VolumeID, rel)
		if !online {
			return nil, fmt.Errorf("「%s」所在的磁盘不在线，接入后再排除", f.Name)
		}
		if k := normPath(abs); !seen[k] {
			seen[k] = true
			fs = append(fs, folder{f.VolumeID, rel, abs})
		}
	}
	// 已经包含在另一个要排除的文件夹里的不用单独列
	var top []folder
	for i, x := range fs {
		inner := false
		for j, y := range fs {
			if i != j && x.vol == y.vol && x.rel != y.rel && nestedIn(x.rel, y.rel) {
				inner = true
			}
		}
		if !inner {
			top = append(top, x)
		}
	}
	fs = top
	if len(fs) == 0 {
		return nil, errors.New("没有可以排除的文件夹")
	}
	cfg := m.Config()
	have := map[string]bool{}
	for _, p := range cfg.ExcludePrefixes {
		have[normPath(p)] = true
	}
	var out []string
	for _, x := range fs {
		out = append(out, x.abs)
		if !have[normPath(x.abs)] {
			cfg.ExcludePrefixes = append(cfg.ExcludePrefixes, x.abs)
		}
	}
	for _, x := range fs {
		n, err := m.DB.DeleteUnder(x.vol, x.rel)
		if err != nil {
			return nil, err
		}
		m.logf("排除文件夹 %s，从索引移除 %d 个文件", logx.P(x.abs), n)
	}
	if err := m.SetConfig(cfg); err != nil {
		return nil, err
	}
	return out, nil
}
