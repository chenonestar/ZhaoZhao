package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.log")
	r, err := New(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for i := 0; i < 30; i++ {
		r.Write([]byte(strings.Repeat("x", 30) + "\n"))
	}
	if st, err := os.Stat(p); err != nil || st.Size() > 100 {
		t.Fatalf("%s: %v", p, err)
	}
	for _, f := range []string{p + ".1.gz", p + ".2.gz"} {
		b, err := ReadLog(f)
		if err != nil || len(b) == 0 || len(b) > 100 || !strings.HasPrefix(string(b), "xxx") {
			t.Fatalf("%s: %v %q", f, err, b)
		}
	}
	for _, f := range []string{p + ".1", p + ".2", p + ".3", p + ".3.gz"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("不应有 %s（旧文件应压缩，且不超过 keep 个）", f)
		}
	}
}

// 0.1.x 留下的未压缩旧日志：打开时在后台压缩。
func TestCompressLegacyLogs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	os.WriteFile(p+".1", []byte("旧日志一\n"), 0o600)
	os.WriteFile(p+".2", []byte("旧日志二\n"), 0o600)
	r, err := New(p, 1<<20, 5)
	if err != nil {
		t.Fatal(err)
	}
	r.Write([]byte("x\n"))
	r.Close() // 等后台压缩完成
	for i, want := range []string{"旧日志一", "旧日志二"} {
		f := p + "." + itoa(i+1)
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s 应已压缩", f)
		}
		if b, err := ReadLog(f + ".gz"); err != nil || !strings.Contains(string(b), want) {
			t.Errorf("%s.gz: %v %q", f, err, b)
		}
	}
	all := Files{Log: p, Keep: 5}.AllLogs()
	if len(all) != 3 {
		t.Errorf("AllLogs %v", all)
	}
}
