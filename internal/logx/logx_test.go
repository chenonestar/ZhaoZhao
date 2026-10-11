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
	for _, f := range []string{p, p + ".1", p + ".2"} {
		if st, err := os.Stat(f); err != nil || st.Size() > 100 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("不应保留超过 keep 个旧文件")
	}
}
