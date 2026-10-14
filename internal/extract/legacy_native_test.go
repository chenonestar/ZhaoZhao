package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

func legacyFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", "legacy", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("缺少样本 %s（用 tools/make_legacy_fixtures.py 生成）", name)
	}
	return p
}

// copyAs 把样本复制成另一个扩展名（WPS 文件与 Office 老格式结构相同；.doc 其实是 RTF 等）。
func copyAs(t *testing.T, name, as string) string {
	t.Helper()
	b, err := os.ReadFile(legacyFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), as)
	os.WriteFile(p, b, 0o600)
	return p
}

func mustContain(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(text, s) {
			t.Errorf("%s 缺少 %q", what, s)
		}
	}
}

func TestDocNative(t *testing.T) {
	r, err := extractDocNative(legacyFixture(t, "合同.doc"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "合同.doc", r.Text, "采购合同", "违约金为合同总额的百分之五", "HT-2025-0888", "BX-20250101", "差旅费",
		"找找官方网站", "页眉机密文字", "页脚第一页", "第59条", "海獭")
	if strings.Contains(r.Text, "HYPERLINK") {
		t.Error("不应包含域代码")
	}
	if r.Locs[0].Label != "第1段" {
		t.Errorf("locs %v", r.Locs[:3])
	}
	r, err = extractDocNative(legacyFixture(t, "脚注.doc"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "脚注.doc", r.Text, "脚注样本正文", "鸬鹚", "鹈鹕", "犰狳")
	t.Logf("脚注.doc：%q", r.Text)
	var labels []string
	for _, l := range r.Locs {
		labels = append(labels, l.Label)
	}
	t.Logf("labels %v", labels)
	_ = doc.StatusOK
}

func TestXlsNative(t *testing.T) {
	r, err := extractXlsNative(legacyFixture(t, "报销.xls"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "报销.xls", r.Text, "姓名", "张三", "1234.5", "2023-05-01", "2024-12-31 18:30:00", "2469", "公式文字结果",
		"差旅费", "隐藏的关键词", "请核对发票", "共享字符串第0项", "共享字符串第3499项：中文与English混排24493", "穿山甲")
	if r.Locs[0].Label != "报销!A1" {
		t.Errorf("locs %v", r.Locs[:2])
	}
	lines := strings.Split(r.Text, "\n")
	t.Logf("前几行: %q", lines[:4])
}

func TestPptNative(t *testing.T) {
	r, err := extractPptNative(legacyFixture(t, "汇报.ppt"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "汇报.ppt", r.Text, "季度汇报", "营收增长百分之二十", "讲稿备注：强调增长", "第二页标题", "树袋熊")
	for _, bad := range []string{"母版", "Click to edit", "单击此处"} {
		if strings.Contains(r.Text, bad) {
			t.Errorf("不应包含母版文字 %q", bad)
		}
	}
	if strings.Count(r.Text, "季度汇报") != 1 {
		t.Errorf("文字重复：%q", r.Text)
	}
	var labels []string
	for _, l := range r.Locs {
		labels = append(labels, l.Label)
	}
	t.Logf("%q %v", r.Text, labels)
}

func TestRtfNative(t *testing.T) {
	r := extractRTF(legacyFixture(t, "合同.rtf"), DefaultOptions())
	mustContain(t, "合同.rtf", r.Text, "采购合同", "违约金为合同总额的百分之五", "HT-2025-0888", "BX-20250101", "差旅费",
		"找找官方网站", "页眉机密文字", "页脚第一页", "第59条", "海獭")
	for _, bad := range []string{"HYPERLINK", "SimSun", "Times New Roman", "rsid", "Normal"} {
		if strings.Contains(r.Text, bad) {
			t.Errorf("合同.rtf 不应包含 %q", bad)
		}
	}
	if strings.Count(r.Text, "海獭") != 1 {
		t.Errorf("文字重复")
	}
	r = extractRTF(legacyFixture(t, "gbk.rtf"), DefaultOptions())
	mustContain(t, "gbk.rtf", r.Text, "国标码文字：麒麟", "Unicode 猪猫 end")
	t.Logf("gbk.rtf: %q", r.Text)
	if strings.Contains(r.Text, "?") {
		t.Errorf("\\uN 后的替代字符应跳过：%q", r.Text)
	}
}

// TestLegacyDispatch 按实际内容选择解析器：WPS 文件、扩展名与内容不符、内置解析器处理不了的文件。
func TestLegacyDispatch(t *testing.T) {
	opt := DefaultOptions()
	opt.LegacyMode = LegacyNative
	cases := []struct {
		src, as string
		want    []string
	}{
		{"合同.doc", "合同.wps", []string{"采购合同", "海獭"}},
		{"报销.xls", "报销.et", []string{"穿山甲", "2023-05-01"}},
		{"汇报.ppt", "汇报.dps", []string{"季度汇报", "树袋熊"}},
		{"合同.rtf", "其实是rtf.doc", []string{"采购合同", "海獭"}},
		{"其实是网页.xls", "其实是网页.xls", []string{"网页表格里的关键词", "獾"}},
		{"合同.doc", "改了扩展名.docx", []string{"采购合同"}},
		{"报销.xls", "改了扩展名.xlsx", []string{"穿山甲"}},
	}
	for _, c := range cases {
		p := copyAs(t, c.src, c.as)
		r := Extract(p, Ext(c.as), opt)
		if r.Status != doc.StatusOK {
			t.Errorf("%s：%s %s", c.as, r.Status, r.Err)
			continue
		}
		mustContain(t, c.as, r.Text, c.want...)
	}
	// 内置解析器处理不了：只用内置解析时交给主程序排队转换
	bad := filepath.Join(t.TempDir(), "坏.doc")
	os.WriteFile(bad, append(append([]byte{}, oleMagic...), make([]byte, 600)...), 0o600)
	if r := Extract(bad, "doc", opt); !r.NeedConvert || r.Status != doc.StatusFailed {
		t.Errorf("坏.doc：%+v", r)
	}
	unknown := filepath.Join(t.TempDir(), "未知.xls")
	os.WriteFile(unknown, []byte("ID;PWXL;N;E\nC;Y1;X1;K\"SYLK\"\nE\n"), 0o600)
	if r := Extract(unknown, "xls", opt); !r.NeedConvert {
		t.Errorf("未知.xls：%+v", r)
	}
	if SofficePath() == "" {
		t.Skip("没有 LibreOffice")
	}
	opt.LegacyMode = LegacyLO
	r := Extract(legacyFixture(t, "汇报.ppt"), "ppt", opt)
	mustContain(t, "LibreOffice 转换的汇报.ppt", r.Text, "季度汇报", "树袋熊")
}
