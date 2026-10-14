package extract

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 内置解析器读的是来历不明的文件：任何输入都不能崩溃、卡死或占用大量内存。
// 平时只跑种子样本；排查时用 go test -fuzz=FuzzLegacyNative -fuzztime=60s ./internal/extract 长时间随机测试。
func FuzzLegacyNative(f *testing.F) {
	for _, n := range []string{"合同.doc", "脚注.doc", "报销.xls", "汇报.ppt", "合同.rtf", "gbk.rtf", "其实是网页.xls"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", n))
		if err != nil {
			f.Fatal(err)
		}
		if len(b) > 64<<10 && filepath.Ext(n) != ".rtf" {
			// 只保留开头（含文件头和目录）以加快变异；完整样本已由其他测试覆盖
			f.Add(b[:64<<10])
		}
		f.Add(b)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(dir, "x.bin")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			extractLegacyNative(p, "doc", DefaultOptions())
			extractLegacyNative(p, "xls", DefaultOptions())
			extractLegacyNative(p, "ppt", DefaultOptions())
			extractRTF(p, DefaultOptions())
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("解析超过 10 秒")
		}
	})
}
