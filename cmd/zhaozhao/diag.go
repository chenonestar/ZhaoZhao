package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/logx"
)

// environment 返回运行环境说明（启动时写日志、导出诊断信息时使用）。路径已用 logx.P 标记。
func environment(dataDir string) string {
	exe, _ := os.Executable()
	return fmt.Sprintf("版本 %s；%s；程序 %s（%s）；数据目录 %s（%s）", version, sysInfo(),
		logx.P(exe), driveKind(exe), logx.P(dataDir), driveKind(dataDir))
}

// exportDiagnostics 把日志、当前状态、设置和运行环境打包成 zip，放在数据目录下。
// anonymize 为真时，所有文件名和路径替换成代号（保留盘符、层级和扩展名）。
func exportDiagnostics(dataDir string, lg *logx.Logger, mgr *index.Manager, anonymize bool) (string, error) {
	lg.Flush()
	name := "zhaozhao-diagnostics-" + time.Now().Format("20060102-150405") + ".zip"
	p := filepath.Join(dataDir, name)
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(f)
	clean := func(s string) string {
		if anonymize {
			return logx.Anonymize(s)
		}
		return s
	}
	add := func(n, content string) error {
		w, err := zw.Create(n)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, clean(content))
		return err
	}

	var sys strings.Builder
	fmt.Fprintf(&sys, "导出时间：%s\n运行环境：%s\nGo：%s\n", time.Now().Format("2006-01-02 15:04:05"), environment(dataDir), runtime.Version())
	hk := currentHotkey()
	fmt.Fprintf(&sys, "全局快捷键：%q（%s）\n", hk.Hotkey, hk.Message)
	if anonymize {
		sys.WriteString("文件名和路径已替换成代号。\n")
	}
	comps, _ := mgr.Components()
	for _, c := range comps {
		fmt.Fprintf(&sys, "组件：%s：%s %s\n", c.Name, c.Message, logx.P(c.Path))
	}
	c := mgr.Config()
	prefixes := make([]string, len(c.ExcludePrefixes))
	for i, x := range c.ExcludePrefixes {
		prefixes[i] = logx.P(x) // 排除的路径前缀也要能被匿名化
	}
	c.ExcludePrefixes = prefixes
	cfg, _ := json.MarshalIndent(c, "", "  ")
	// 设置里的路径是 JSON 转义过的（\\），还原成普通路径以便识别和替换
	fmt.Fprintf(&sys, "\n设置：\n%s\n", strings.ReplaceAll(string(cfg), `\\`, `\`))
	if err := add("系统与设置.txt", sys.String()); err != nil {
		f.Close()
		return "", err
	}

	st := mgr.Status()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n索引根：\n", mgr.Snapshot())
	for _, r := range st.Roots {
		fmt.Fprintf(&b, "  %s（%s，在线 %v，扫描中 %v）文件 %d，待处理 %d，失败 %d\n", logx.P(r.Path), index.KindName(r.Kind), r.Online, r.Scanning, r.Counts.Total, r.Counts.Pending, r.Counts.Failed)
	}
	fmt.Fprintf(&b, "\n磁盘：\n")
	for _, v := range st.Volumes {
		fmt.Fprintf(&b, "  %s（%s）在线 %v %s 文件 %d\n", v.ID, index.KindName(v.Kind), v.Online, logx.P(v.Mount), v.Files)
	}
	if fl, err := mgr.DB.ListFailed(500); err == nil && len(fl) > 0 {
		fmt.Fprintf(&b, "\n失败列表（最多 500 个）：\n")
		for _, x := range fl {
			path, _ := mgr.AbsPath(x.VolumeID, x.RelPath)
			if x.Inner != "" {
				path += " › " + x.Inner
			}
			fmt.Fprintf(&b, "  %s：%s\n", logx.P(path), x.Err)
		}
	}
	if err := add("当前状态.txt", b.String()); err != nil {
		f.Close()
		return "", err
	}

	if sp, err := mgr.SpaceStats(); err == nil {
		if err := add("索引空间.txt", index.SpaceText(sp)); err != nil {
			f.Close()
			return "", err
		}
	}
	if lt := mgr.LegacyTest(); lt.Report != nil {
		if err := add("老格式测试.txt", index.LegacyTestText(lt.Report, false)); err != nil {
			f.Close()
			return "", err
		}
	}

	for _, lp := range lg.AllLogs() {
		data, err := os.ReadFile(lp)
		if err != nil {
			continue
		}
		if err := add("日志/"+filepath.Base(lp), string(data)); err != nil {
			f.Close()
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return "", err
	}
	return p, f.Close()
}
