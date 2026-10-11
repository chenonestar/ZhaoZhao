package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/index"
)

// tempCache 管理容器内文件解出的临时只读副本；程序退出时清理。
type tempCache struct {
	mu  sync.Mutex
	dir string
	seq int
}

func newTempCache() *tempCache {
	d, err := os.MkdirTemp("", "zhaozhao-open-")
	if err != nil {
		d = filepath.Join(os.TempDir(), "zhaozhao-open")
		_ = os.MkdirAll(d, 0o700)
	}
	return &tempCache{dir: d}
}

// Open 把容器（压缩包、邮件）里的 inner 条目解出到临时目录并返回路径。
func (t *tempCache) Open(container, inner string, cfg index.Config) (string, error) {
	t.mu.Lock()
	t.seq++
	sub := filepath.Join(t.dir, fmt.Sprint(t.seq))
	t.mu.Unlock()
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", err
	}
	opts := extract.DefaultOptions()
	opts.MaxDepth = cfg.ContainerMaxDepth
	p, err := extract.ExtractInner(container, inner, sub, opts)
	if err != nil {
		return "", err
	}
	_ = os.Chmod(p, 0o444)
	return p, nil
}

func (t *tempCache) Cleanup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = filepath.WalkDir(t.dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(t.dir)
}
