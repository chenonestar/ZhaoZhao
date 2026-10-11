// Package doc 定义提取结果的公共类型。
package doc

import (
	"strings"
	"unicode/utf8"
)

// Loc 标记从 Off（字符偏移）开始的内容所在位置，例如「第 3 页」「Sheet1!B2」。
type Loc struct {
	Off   int    `json:"o"`
	Label string `json:"l"`
}

// Status 是提取结果状态。
const (
	StatusOK        = "ok"
	StatusEncrypted = "encrypted"
	StatusFailed    = "failed"
	StatusNoContent = "nocontent" // 没有可提取的内容（如无文字层的扫描件）
	StatusSkipped   = "skipped"   // 不支持/被配置跳过，只保留文件名
)

// Result 是单个文件的提取结果。
type Result struct {
	Status    string `json:"status"`
	Err       string `json:"err,omitempty"`
	Text      string `json:"text,omitempty"`
	Locs      []Loc  `json:"locs,omitempty"`
	LineBased bool   `json:"lineBased,omitempty"` // 位置以行号表示
	OCR       bool   `json:"ocr,omitempty"`       // 文本含 OCR 结果
	NeedOCR   bool   `json:"needOcr,omitempty"`   // 有页面没有文字层，需要 OCR（本次未做）
	Hash      string `json:"hash,omitempty"`
	Meta      string `json:"meta,omitempty"` // 邮件头等 JSON 元数据
	// Children 是容器（压缩包、邮件）里的内部条目，每个条目独立成为搜索结果。
	Children []Child `json:"children,omitempty"`
}

// Child 是容器内的一个条目。
type Child struct {
	Inner string `json:"inner"` // 容器内路径，如 "合同/采购合同.docx" 或 "邮件标题 › 附件.xlsx"
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Result
}

// Builder 逐段拼接文本并记录位置标记。
type Builder struct {
	sb   strings.Builder
	n    int
	Locs []Loc
	last string
}

func (b *Builder) Mark(label string) {
	if label == b.last {
		return
	}
	b.last = label
	b.Locs = append(b.Locs, Loc{Off: b.n, Label: label})
}

func (b *Builder) WriteString(s string) {
	b.sb.WriteString(s)
	b.n += utf8.RuneCountInString(s)
}

// Line 写入一行（自动补换行）。
func (b *Builder) Line(s string) {
	b.WriteString(s)
	b.WriteString("\n")
}

func (b *Builder) Len() int       { return b.n }
func (b *Builder) String() string { return b.sb.String() }
