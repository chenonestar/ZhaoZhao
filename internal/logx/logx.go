// Package logx 提供按大小滚动的本地日志文件（便于排查问题，数据不外发）。
package logx

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Rotating 是按大小滚动的日志写入器。
type Rotating struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
	bg   sync.WaitGroup // 启动时压缩旧日志
}

// New 打开日志文件。超过 max 字节时滚动，保留 keep 个旧文件。旧文件用 gzip 压缩
// （path.1.gz 最新），文字日志压缩后只有原来的十分之一左右。
func New(path string, max int64, keep int) (*Rotating, error) {
	r := &Rotating{path: path, max: max, keep: keep}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	// 0.1.x 留下的未压缩旧日志：在后台压缩（持有锁，期间的写入稍等，由异步缓冲吸收）
	r.bg.Add(1)
	go func() {
		defer r.bg.Done()
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := 1; i <= r.keep; i++ {
			plain := fmt.Sprintf("%s.%d", r.path, i)
			if _, err := os.Stat(plain); err == nil {
				_ = gzipFile(plain, plain+".gz")
			}
		}
	}()
	return r, nil
}

// gzipFile 把 src 压缩成 dst，成功后删除 src。
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	in.Close()
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}

// ReadLog 读取日志文件，.gz 的自动解压。
func ReadLog(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if filepath.Ext(p) != ".gz" {
		return io.ReadAll(f)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func (r *Rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	r.f, r.size = f, 0
	if st != nil {
		r.size = st.Size()
	}
	return nil
}

func (r *Rotating) rotate() {
	r.f.Close()
	old := func(i int) string { return fmt.Sprintf("%s.%d", r.path, i) }
	_ = os.Remove(old(r.keep) + ".gz")
	_ = os.Remove(old(r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(old(i)+".gz", old(i+1)+".gz")
		_ = os.Rename(old(i), old(i+1)) // 还没压缩的（压缩失败时）
	}
	_ = os.Rename(r.path, old(1))
	_ = r.open()
	// 每 10 MB 压缩一次，约 0.1 秒；写日志的是异步线程，不影响程序其他部分
	_ = gzipFile(old(1), old(1)+".gz")
}

func (r *Rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return len(p), nil
	}
	if r.size+int64(len(p)) > r.max {
		r.rotate()
		if r.f == nil {
			return len(p), nil
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *Rotating) Close() error {
	r.bg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}
