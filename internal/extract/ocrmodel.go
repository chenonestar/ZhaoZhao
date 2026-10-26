package extract

import (
	"os"
	"path/filepath"
	"strings"
)

// OCR 认字模型。检测文字位置、判断方向的模型不变，只换认字的模型。
//
// 组件发布包自带 PP-OCRv4（args.txt 里写的）。打包时另外放入 PP-OCRv6 small 认字模型（Windows 上实测每页
// 快约 12%，模拟扫描页上的字准确率 95% → 99.9%）和它的 INT8 量化版（小数运算换成整数运算，再快一些，
// 但快多少和 CPU 有关，所以测速时按本机实测选，见 index 包的 OCR 测速）。
const (
	OCRModelV6  = "v6"  // PP-OCRv6 small
	OCRModelV6Q = "v6q" // PP-OCRv6 small，INT8 量化
	OCRModelV4  = "v4"  // PP-OCRv4（组件自带）
)

// ocrModelFiles 是各模型在组件 models 目录里的文件：认字模型、字表。v4 用 args.txt 里写的，不在这里。
var ocrModelFiles = map[string][2]string{
	OCRModelV6:  {"PP-OCRv6_rec_small.onnx", "PP-OCRv6_keys.txt"},
	OCRModelV6Q: {"PP-OCRv6_rec_small_int8.onnx", "PP-OCRv6_keys.txt"},
}

// OCRModelName 是界面、日志里显示的模型名。
func OCRModelName(model string) string {
	switch model {
	case OCRModelV6:
		return "PP-OCRv6 small"
	case OCRModelV6Q:
		return "PP-OCRv6 small（量化版）"
	case OCRModelV4:
		return "PP-OCRv4"
	}
	return model
}

// modelsDir 返回组件的模型目录（--models= 参数，相对组件所在目录）。
func (c OCRConfig) modelsDir() string {
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, "--models="); ok {
			if filepath.IsAbs(v) {
				return v
			}
			return filepath.Join(filepath.Dir(c.Command), v)
		}
	}
	return ""
}

// HasModel 说明组件里有没有这个认字模型。v4 是组件自带的，总是有。
func (c OCRConfig) HasModel(model string) bool {
	if model == OCRModelV4 {
		return c.Enabled()
	}
	files, ok := ocrModelFiles[model]
	dir := c.modelsDir()
	if !ok || dir == "" {
		return false
	}
	for _, f := range files {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.IsDir() {
			return false
		}
	}
	return true
}

// Models 返回组件里有的认字模型，按推荐的顺序（新模型在前）。
func (c OCRConfig) Models() []string {
	var out []string
	for _, m := range []string{OCRModelV6, OCRModelV6Q, OCRModelV4} {
		if c.HasModel(m) {
			out = append(out, m)
		}
	}
	return out
}

// WithModel 换成 model 认字（替换 --rec、--keys 参数）。v4、没有这个模型的文件时原样返回。
// 参数不同的 OCR 进程是分开的：换了模型，常驻的进程会换成新的。
func (c OCRConfig) WithModel(model string) OCRConfig {
	files, ok := ocrModelFiles[model]
	if !ok || !c.HasModel(model) {
		return c
	}
	args := make([]string, 0, len(c.Args)+2)
	for _, a := range c.Args {
		if !strings.HasPrefix(a, "--rec=") && !strings.HasPrefix(a, "--keys=") {
			args = append(args, a)
		}
	}
	c.Args = append(args, "--rec="+files[0], "--keys="+files[1])
	return c
}
