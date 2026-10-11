package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// 集成样本由 tools/make_fixtures.py 生成；设置 ZHAOZHAO_FIXTURES 指向其输出目录。
func fixtures(t *testing.T) string {
	d := os.Getenv("ZHAOZHAO_FIXTURES")
	if d == "" {
		t.Skip("未设置 ZHAOZHAO_FIXTURES")
	}
	return d
}

func run(t *testing.T, name string) doc.Result {
	p := filepath.Join(fixtures(t), name)
	r := Extract(p, Ext(name), DefaultOptions())
	t.Logf("%s: status=%s err=%q text=%q locs=%v", name, r.Status, r.Err, r.Text, r.Locs)
	return r
}

func want(t *testing.T, r doc.Result, subs ...string) {
	t.Helper()
	if r.Status != doc.StatusOK {
		t.Fatalf("status=%s err=%s", r.Status, r.Err)
	}
	for _, s := range subs {
		if !strings.Contains(r.Text, s) {
			t.Errorf("缺少 %q", s)
		}
	}
}

func TestDocx(t *testing.T) {
	r := run(t, "合同.docx")
	want(t, r, "违约金为合同总额", "BX-20250101", "页眉机密文字")
}

func TestXlsx(t *testing.T) {
	r := run(t, "报销.xlsx")
	want(t, r, "张三", "1234.5", "差旅费", "隐藏的关键词", "请核对发票")
}

func TestPptx(t *testing.T) {
	want(t, run(t, "汇报.pptx"), "季度汇报", "营收增长", "讲稿备注")
}

func TestPDF(t *testing.T) {
	r := run(t, "说明.pdf")
	want(t, r, "不可抗力条款", "Invoice number 20250707")
	if len(r.Locs) != 2 || r.Locs[0].Label != "第1页" || r.Locs[1].Label != "第2页" {
		t.Errorf("locs %v", r.Locs)
	}
}

func TestEncodings(t *testing.T) {
	want(t, run(t, "gbk.txt"), "GBK编码", "报销事项")
	want(t, run(t, "big5.txt"), "大五碼", "報銷事項")
	want(t, run(t, "utf16.txt"), "UTF-16", "差旅")
	want(t, run(t, "a.py"), "计算税额")
}

func TestLegacy(t *testing.T) {
	if SofficePath() == "" {
		t.Skip("没有 LibreOffice")
	}
	want(t, run(t, "合同.doc"), "违约金")
	want(t, run(t, "报销.xls"), "张三", "隐藏的关键词")
	want(t, run(t, "汇报.ppt"), "季度汇报")
	want(t, run(t, "合同2.wps"), "违约金")
}

func TestEncryptedOLE(t *testing.T) {
	// 以 OLE 复合文档头伪装的 .docx 应识别为已加密
	p := filepath.Join(t.TempDir(), "x.docx")
	os.WriteFile(p, append([]byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"), make([]byte, 100)...), 0o600)
	if r := Extract(p, "docx", DefaultOptions()); r.Status != doc.StatusEncrypted {
		t.Fatalf("%+v", r)
	}
}

func TestSniffUnknown(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "x.weird")
	os.WriteFile(p, []byte("hello 世界\n"), 0o600)
	if r := Extract(p, "weird", DefaultOptions()); r.Status != doc.StatusOK || !strings.Contains(r.Text, "世界") {
		t.Fatalf("%+v", r)
	}
	os.WriteFile(p, []byte{0, 1, 2, 3, 0, 0, 9}, 0o600)
	if r := Extract(p, "weird", DefaultOptions()); r.Status != doc.StatusSkipped {
		t.Fatalf("%+v", r)
	}
}
