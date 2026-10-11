// Package index 负责索引的建立与维护：扫描、监听、调度、卷管理。
package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/norm"
)

// Config 是全局设置，保存为数据目录下的 config.json。
type Config struct {
	ExcludeDirNames []string `json:"excludeDirNames"` // 任意层级的目录名
	ExcludePrefixes []string `json:"excludePrefixes"` // 路径前缀
	ExcludeGlobs    []string `json:"excludeGlobs"`    // glob，匹配文件名或完整路径
	ExcludeExts     []string `json:"excludeExts"`     // 扩展名（不含点）

	MaxTextMB    int  `json:"maxTextMB"`    // 文本类文件大小上限
	MaxOtherMB   int  `json:"maxOtherMB"`   // 其他类型大小上限
	FollowLinks  bool `json:"followLinks"`  // 跟随符号链接和目录联接
	SniffUnknown bool `json:"sniffUnknown"` // 未知扩展名抽样判断是否为文本
	SkipCloud    bool `json:"skipCloud"`    // 跳过云盘仅在线文件

	Simplify bool `json:"simplify"` // 繁简体归一

	Workers           int  `json:"workers"`         // 提取并发数，0 表示 CPU 核心数的一半
	TimeoutSec        int  `json:"timeoutSec"`      // 单文件提取超时
	LegacyWorkers     int  `json:"legacyWorkers"`   // 老格式转换并发数
	NetworkScanMin    int  `json:"networkScanMin"`  // 网络盘定时扫描间隔（分钟）
	NetworkReadConc   int  `json:"networkReadConc"` // 网络盘读取并发数
	PauseOnBattery    bool `json:"pauseOnBattery"`  // 使用电池时暂停 OCR 和格式转换
	ContainerMaxDepth int  `json:"containerMaxDepth"`
	ArchiveMaxMB      int  `json:"archiveMaxMB"`    // 单个压缩包解压总量上限
	ArchiveMaxRatio   int  `json:"archiveMaxRatio"` // 单个内部文件压缩比上限

	AutoDeleteDays int `json:"autoDeleteDays"` // 超过 N 天未接入则删除卷索引，0 关闭
}

// DefaultConfig 返回默认设置，dataDir 会被加入排除列表（软件自身的数据目录）。
func DefaultConfig(dataDir string) Config {
	c := Config{
		ExcludeDirNames:   []string{"node_modules", ".git", ".svn", ".hg", "__pycache__", ".venv", "$Recycle.Bin", "System Volume Information", "$RECYCLE.BIN"},
		ExcludePrefixes:   defaultPrefixes(),
		ExcludeExts:       []string{},
		MaxTextMB:         100,
		MaxOtherMB:        200,
		SniffUnknown:      true,
		SkipCloud:         true,
		Simplify:          true,
		TimeoutSec:        60,
		LegacyWorkers:     2,
		NetworkScanMin:    60,
		NetworkReadConc:   2,
		PauseOnBattery:    true,
		ContainerMaxDepth: 3,
		ArchiveMaxMB:      2048,
		ArchiveMaxRatio:   100,
	}
	if dataDir != "" {
		c.ExcludePrefixes = append(c.ExcludePrefixes, dataDir)
	}
	return c
}

func defaultPrefixes() []string {
	var p []string
	if runtime.GOOS == "windows" {
		p = append(p, `C:\Windows`, `C:\Program Files`, `C:\Program Files (x86)`, `C:\ProgramData`)
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			p = append(p, filepath.Join(l, "Temp"))
		}
		for _, f := range []string{"pagefile.sys", "hiberfil.sys", "swapfile.sys"} {
			p = append(p, `C:\`+f)
		}
	} else {
		p = append(p, "/proc", "/sys", "/dev", "/run")
	}
	return p
}

// OSExclusions 在配置的排除前缀之外，补上与卷无关的系统文件名（各盘根目录下）。
var systemFiles = map[string]bool{"pagefile.sys": true, "hiberfil.sys": true, "swapfile.sys": true}

// Simplified 返回当前归一化选项。
func (c Config) NormOptions() norm.Options { return norm.Options{Simplify: c.Simplify} }

func (c Config) workers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	return n
}

// RootOpts 是索引根自己的设置（F-IDX-02），为空则使用全局设置。
type RootOpts struct {
	IncludeExts  []string `json:"includeExts,omitempty"`  // 只索引这些类型的内容，空表示不限
	ExcludeGlobs []string `json:"excludeGlobs,omitempty"` // 追加排除规则
	FollowLinks  *bool    `json:"followLinks,omitempty"`
	OCR          *bool    `json:"ocr,omitempty"` // 图片 OCR 开关
}

func parseRootOpts(raw json.RawMessage) RootOpts {
	var o RootOpts
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &o)
	}
	return o
}

// Rules 是编译后的排除规则。
type Rules struct {
	dirNames map[string]bool
	prefixes []string
	globs    []string
	exts     map[string]bool
	include  map[string]bool
}

func normPath(p string) string {
	p = strings.ToLower(filepath.Clean(p))
	return strings.TrimRight(p, `\/`)
}

func newRules(c Config, o RootOpts) *Rules {
	r := &Rules{dirNames: map[string]bool{}, exts: map[string]bool{}}
	for _, n := range c.ExcludeDirNames {
		r.dirNames[strings.ToLower(n)] = true
	}
	for _, p := range c.ExcludePrefixes {
		if p != "" {
			r.prefixes = append(r.prefixes, normPath(os.ExpandEnv(p)))
		}
	}
	r.globs = append(append([]string{}, c.ExcludeGlobs...), o.ExcludeGlobs...)
	for _, e := range c.ExcludeExts {
		r.exts[strings.ToLower(strings.TrimPrefix(e, "."))] = true
	}
	if len(o.IncludeExts) > 0 {
		r.include = map[string]bool{}
		for _, e := range o.IncludeExts {
			r.include[strings.ToLower(strings.TrimPrefix(e, "."))] = true
		}
	}
	return r
}

func (r *Rules) underPrefix(abs string) bool {
	n := normPath(abs)
	for _, p := range r.prefixes {
		if n == p || strings.HasPrefix(n, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (r *Rules) globMatch(abs, name string) bool {
	for _, g := range r.globs {
		if ok, _ := filepath.Match(strings.ToLower(g), strings.ToLower(name)); ok {
			return true
		}
		if ok, _ := filepath.Match(strings.ToLower(g), strings.ToLower(abs)); ok {
			return true
		}
	}
	return false
}

// SkipDir 判断整个目录是否排除。
func (r *Rules) SkipDir(abs, name string) bool {
	return r.dirNames[strings.ToLower(name)] || r.underPrefix(abs) || r.globMatch(abs, name)
}

// SkipFile 判断文件是否完全排除（连文件名也不索引）。
func (r *Rules) SkipFile(abs, name, ext string) bool {
	if systemFiles[strings.ToLower(name)] {
		return true
	}
	return r.exts[ext] || r.underPrefix(abs) || r.globMatch(abs, name)
}

// ContentAllowed 判断文件内容是否应提取（索引根可限定类型，F-IDX-02）。
func (r *Rules) ContentAllowed(ext string) bool {
	return r.include == nil || r.include[ext]
}

// 文件大小上限判断（F-IDX-06）：文本类默认 100 MB，其他 200 MB。
func (c Config) sizeLimit(ext string) int64 {
	k := extract.KindOf(ext)
	mb := c.MaxOtherMB
	if k == extract.KindText || k == extract.KindCode || k == extract.KindOther {
		mb = c.MaxTextMB
	}
	return int64(mb) << 20
}
