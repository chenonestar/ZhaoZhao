package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/volume"
)

// 衡量详细日志对索引速度的影响：同样 3000 个小文件，分别用「详细」和「普通」日志建立索引，
// 日志写到真实文件。设置 ZZ_LOGCOST=1 时运行（CI 默认跳过）。
func TestLogCost(t *testing.T) {
	if os.Getenv("ZZ_LOGCOST") == "" {
		t.Skip("设置 ZZ_LOGCOST=1 运行")
	}
	dir := t.TempDir()
	for i := 0; i < 3000; i++ {
		write(t, filepath.Join(dir, fmt.Sprintf("d%02d", i%30), fmt.Sprintf("f%04d.txt", i)), fmt.Sprintf("第 %d 个文件 内容 %d", i, i*7))
	}
	run := func(level string) (time.Duration, int64) {
		logDir := t.TempDir()
		lg, err := logx.Setup(logDir)
		if err != nil {
			t.Fatal(err)
		}
		defer lg.Close()
		prov := &fakeProv{}
		prov.addKind("v1", "本地", dir, true, volume.KindLocal)
		e := newEnvWith(t, prov, fmt.Sprintf(`{"logLevel":%q,"configVersion":2}`, level), nil)
		start := time.Now()
		if _, err := e.m.AddRoot(dir); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "全部完成", 5*time.Minute, func() bool {
			c, _ := e.m.DB.AllCounts()
			return c.Total == 3000 && c.Pending == 0
		})
		d := time.Since(start)
		lg.Flush()
		st, _ := os.Stat(filepath.Join(logDir, "zhaozhao.log"))
		return d, st.Size()
	}
	for round := 0; round < 2; round++ {
		dn, sn := run("normal")
		dd, sd := run("detail")
		t.Logf("第 %d 轮：普通日志 %v（日志 %d KB），详细日志 %v（日志 %d KB），差 %+.1f%%",
			round+1, dn.Round(time.Millisecond), sn>>10, dd.Round(time.Millisecond), sd>>10, (dd.Seconds()/dn.Seconds()-1)*100)
	}
}
