package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/sysinfo"
)

// environment 返回运行环境说明（启动时写日志、导出诊断信息时使用）。路径已用 logx.P 标记。
func environment(dataDir string) string {
	exe, _ := os.Executable()
	return fmt.Sprintf("版本 %s；%s；程序 %s（%s）；数据目录 %s（%s）", version, sysInfo(),
		logx.P(exe), driveKind(exe), logx.P(dataDir), diskDesc(dataDir))
}

// diskDesc 说明路径所在的盘：本地盘、移动盘还是网络盘，固态硬盘还是机械硬盘（取得到时），如「本地盘，机械硬盘，SATA」。
// 索引库在机械硬盘上时写入、整理都慢得多，诊断时要知道。
func diskDesc(p string) string {
	d := driveKind(p)
	if m := sysinfo.DiskMedia(p); m != "" {
		d += "，" + m
	}
	return d
}

// diagSpaceLimit：导出诊断信息时空间统计最多花多久（大索引库要读遍整个文件，机械硬盘上冷读 1.5 GB 的
// 索引库要一分多钟），超时就写一份不用读遍索引库的简要统计。这次运行里已经统计过（设置页点过「统计详情」）
// 时直接用那次的结果。
var diagSpaceLimit = 2 * time.Minute

// exportDiagnostics 把日志、当前状态、设置和运行环境打包成 zip，放在数据目录下。
// anonymize 为真时，所有文件名和路径替换成代号（保留盘符、层级和扩展名）。progress 报告进行到哪一步。
// 先写成 .part 文件，全部写完才改名：中途出错不会留下打不开的 zip。
func exportDiagnostics(dataDir string, lg *logx.Logger, mgr *index.Manager, anonymize bool, progress func(string)) (string, error) {
	progress("整理日志")
	lg.Flush()
	name := "zhaozhao-diagnostics-" + time.Now().Format("20060102-150405") + ".zip"
	p := filepath.Join(dataDir, name)
	f, err := os.Create(p + ".part")
	if err != nil {
		return "", err
	}
	defer os.Remove(p + ".part") // 成功时已改名，删除不到
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

	progress("系统与设置")
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
	oi := mgr.OCRBenchInfo()
	fmt.Fprintf(&sys, "OCR：现在 %s，内存允许 %d 个进程", oi.Plan, oi.MemCap)
	if oi.Bench != nil {
		fmt.Fprintf(&sys, "；本机测速（%s）：%s", time.Unix(oi.Bench.At, 0).Format("2006-01-02 15:04"), oi.Bench.Summary())
	}
	if oi.Note != "" {
		fmt.Fprintf(&sys, "；%s", oi.Note)
	}
	sys.WriteString("\n")
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

	progress("当前状态和失败列表")
	st := mgr.Status()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n索引根：\n", mgr.Snapshot())
	for _, r := range st.Roots {
		fmt.Fprintf(&b, "  %s（%s，在线 %v，扫描中 %v）文件 %d，待处理 %d，失败 %d\n", logx.P(r.Path), index.KindName(r.Kind), r.Online, r.Scanning, r.Counts.Total, r.Counts.Pending, r.Counts.Failed)
	}
	fmt.Fprintf(&b, "\n磁盘：\n")
	for _, v := range st.Volumes {
		kind := index.KindName(v.Kind)
		if m := sysinfo.DiskMedia(v.Mount); v.Online && m != "" {
			kind += "，" + m
		}
		fmt.Fprintf(&b, "  %s（%s）在线 %v %s 文件 %d\n", v.ID, kind, v.Online, logx.P(v.Mount), v.Files)
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

	space := ""
	if sp, at := mgr.LastSpaceStats(); sp != nil {
		space = fmt.Sprintf("（%s 统计的结果）\n", at.Format("2006-01-02 15:04:05")) + index.SpaceText(sp)
	} else {
		progress(fmt.Sprintf("索引库空间统计（机械硬盘上较慢，最多 %d 分钟）", int(diagSpaceLimit.Minutes())))
		ctx, cancel := context.WithTimeout(context.Background(), diagSpaceLimit)
		sp, err := mgr.SpaceStatsContext(ctx)
		cancel()
		switch {
		case err == nil:
			space = index.SpaceText(sp)
		case errors.Is(err, context.DeadlineExceeded):
			space = fmt.Sprintf("完整的空间统计超过 %d 分钟，改写简要统计：\n", int(diagSpaceLimit.Minutes())) + mgr.QuickSpaceText()
		default:
			space = "空间统计失败：" + err.Error() + "\n" + mgr.QuickSpaceText()
		}
	}
	if err := add("索引空间.txt", space); err != nil {
		f.Close()
		return "", err
	}
	if lt := mgr.LegacyTest(); lt.Report != nil {
		if err := add("老格式测试.txt", index.LegacyTestText(lt.Report, false)); err != nil {
			f.Close()
			return "", err
		}
	}

	logs := lg.AllLogs()
	for i, lp := range logs {
		progress(fmt.Sprintf("日志 %d/%d", i+1, len(logs)))
		data, err := logx.ReadLog(lp) // 旧日志是压缩的：解压后才能隐藏路径
		if err != nil {
			continue
		}
		if err := add("日志/"+strings.TrimSuffix(filepath.Base(lp), ".gz"), string(data)); err != nil {
			f.Close()
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return p, os.Rename(p+".part", p)
}
