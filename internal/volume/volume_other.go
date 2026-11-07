//go:build !windows

package volume

import (
	"path/filepath"
	"strings"
)

type systemProvider struct{}

// System 返回当前系统的卷提供者。非 Windows 平台只有一个卷，仅用于开发和测试。
func System() Provider { return systemProvider{} }

var rootInfo = Info{ID: "unix:root", Label: "/", Kind: KindLocal, Mount: "/"}

func (systemProvider) Detect(path string) (Info, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Info{}, "", err
	}
	return rootInfo, strings.Trim(filepath.ToSlash(abs), "/"), nil
}

func (systemProvider) List() []Info { return []Info{rootInfo} }
