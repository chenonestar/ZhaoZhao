// Package volume 识别磁盘卷，使索引路径不依赖盘符（需求 F-VOL-01、F-NET-01）。
package volume

// Kind 是卷的类型。
const (
	KindLocal     = "local"
	KindRemovable = "removable"
	KindNetwork   = "network"
)

// Info 描述一个卷。ID 在盘符变化后保持不变。
type Info struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Serial   string `json:"serial"`
	GUID     string `json:"guid"`
	Capacity int64  `json:"capacity"`
	Kind     string `json:"kind"`
	Mount    string `json:"mount"` // 当前挂载点：盘符根目录或 UNC 共享根
}

// Provider 提供卷信息。生产环境用 System()，测试可替换为假实现。
type Provider interface {
	// Detect 返回路径所在的卷，以及路径相对卷挂载点的部分（用 / 分隔，无前导斜杠）。
	Detect(path string) (Info, string, error)
	// List 返回当前在线的全部卷。
	List() []Info
}
