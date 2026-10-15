package extract

import (
	"bytes"
	"crypto/rc4"
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	t.Cleanup(CleanupWorker)
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

// TestResidentLO 常驻 LibreOffice：连续转换多个文件只启动一次；转换失败不影响后续文件。
func TestResidentLO(t *testing.T) {
	if SofficePath() == "" {
		t.Skip("没有 LibreOffice")
	}
	t.Cleanup(CleanupWorker)
	opt := DefaultOptions()
	opt.LegacyMode = LegacyLO
	files := []struct{ name, want string }{
		{"合同.doc", "海獭"}, {"报销.xls", "穿山甲"}, {"汇报.ppt", "树袋熊"}, {"脚注.doc", "鸬鹚"},
	}
	start := time.Now()
	for i, f := range files {
		t0 := time.Now()
		r := Extract(legacyFixture(t, f.name), Ext(f.name), opt)
		if r.Status != doc.StatusOK {
			t.Fatalf("%s：%s %s", f.name, r.Status, r.Err)
		}
		mustContain(t, f.name, r.Text, f.want)
		t.Logf("%d %s 用时 %.2fs", i, f.name, time.Since(t0).Seconds())
	}
	if loDisabled != "" {
		t.Logf("常驻方式不可用（%s），使用了单独启动", loDisabled)
	} else if loCur == nil || loCur.n != len(files) {
		t.Errorf("应只启动一次常驻 LibreOffice")
	}
	// 损坏的文件：失败，但常驻进程继续可用
	bad := filepath.Join(t.TempDir(), "坏.doc")
	os.WriteFile(bad, []byte("not a document at all"), 0o600)
	r := Extract(bad, "doc", opt)
	t.Logf("坏.doc：%s %s", r.Status, r.Err)
	r = Extract(legacyFixture(t, "合同.doc"), "doc", opt)
	mustContain(t, "再次转换", r.Text, "海獭")
	t.Logf("合计 %.2fs", time.Since(start).Seconds())
}

func TestCheckSofficeResident(t *testing.T) {
	if SofficePath() == "" {
		t.Skip("没有 LibreOffice")
	}
	st := CheckSoffice()
	t.Logf("%+v", st)
	if !st.OK {
		t.Fatal(st.Message)
	}
	if loScript != "" {
		t.Error("自检用的桥接脚本应删除")
	}
}

// 「保护工作簿」的 .xls 用默认密码加密，打开不需要密码：内置解析器应能读出内容；
// 真正设了打开密码的才算已加密。样本由 LibreOffice 生成（标准 RC4），已用 msoffcrypto 独立核对。
func TestXlsDefaultPassword(t *testing.T) {
	opt := DefaultOptions()
	opt.LegacyMode = LegacyNative
	for _, as := range []string{"保护工作簿.xls", "保护工作簿.et"} {
		r := Extract(copyAs(t, "保护工作簿.xls", as), Ext(as), opt)
		if r.Status != doc.StatusOK || !strings.Contains(r.Text, "狐獴") || !strings.Contains(r.Text, "8848") {
			t.Errorf("%s：%+v", as, r)
		}
	}
	r := Extract(legacyFixture(t, "有打开密码.xls"), "xls", opt)
	if r.Status != doc.StatusEncrypted || strings.Contains(r.Text, "穿山甲") {
		t.Errorf("有打开密码.xls 应为已加密：%+v", r)
	}
}

// Excel 2003 以后默认用 RC4 CryptoAPI 加密：把普通工作簿按同样的方式加密后再读。
// （算法已用 Apache POI 的真实 Excel 加密样本与 msoffcrypto 逐字节核对过。）
func TestXlsCryptoAPIDefaultPassword(t *testing.T) {
	st, _, err := oleStreams(legacyFixture(t, "报销.xls"), "Workbook")
	if err != nil {
		t.Fatal(err)
	}
	enc := encryptXlsCryptoAPI(t, st["Workbook"], xlsDefaultPassword)
	r, err := xlsText(enc)
	if err != nil || !strings.Contains(r.Text, "穿山甲") || !strings.Contains(r.Text, "隐藏的关键词") {
		t.Fatalf("%v %.200q", err, r.Text)
	}
	if r.Locs[0].Label != "报销!A1" {
		t.Errorf("工作表名应正确（BoundSheet 的位置字段不加密）：%v", r.Locs[0])
	}
	if _, err := xlsText(encryptXlsCryptoAPI(t, st["Workbook"], "别的密码")); err != errNativeEncrypted {
		t.Errorf("非默认密码应为已加密：%v", err)
	}
}

// encryptXlsCryptoAPI 按 MS-OFFCRYPTO 的 RC4 CryptoAPI 方式加密 Workbook 流（128 位密钥）。
func encryptXlsCryptoAPI(t *testing.T, plain []byte, password string) []byte {
	t.Helper()
	salt := []byte("0123456789abcdef")
	h0 := sha1.Sum(append(append([]byte{}, salt...), utf16leBytes(password)...))
	key := func(block uint32) []byte {
		b := append(append([]byte{}, h0[:]...), byte(block), byte(block>>8), byte(block>>16), byte(block>>24))
		h := sha1.Sum(b)
		return h[:16]
	}
	verifier := []byte("verifier-16bytes")
	vh := sha1.Sum(verifier)
	c, _ := rc4.NewCipher(key(0))
	encV := make([]byte, 16)
	c.XORKeyStream(encV, verifier)
	encH := make([]byte, 20)
	c.XORKeyStream(encH, vh[:])
	le := func(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
	csp := utf16leBytes("Microsoft Enhanced Cryptographic Provider v1.0\x00")
	hdr := bytes.Join([][]byte{le(4), le(0), le(0x6801), le(0x8004), le(128), le(1), le(0), le(0), csp}, nil)
	fp := bytes.Join([][]byte{{1, 0}, {4, 0, 2, 0}, le(4), le(len(hdr)), hdr, le(16), salt, encV, le(20), encH}, nil)
	rec := append([]byte{0x2F, 0, byte(len(fp)), byte(len(fp) >> 8)}, fp...)
	// 插在第一个 BOF 之后；工作表位置（BoundSheet8 的 lbPlyPos）随之后移
	bofLen := 4 + le16(plain, 2)
	s := append(append(append([]byte{}, plain[:bofLen]...), rec...), plain[bofLen:]...)
	for off := 0; off+4 <= len(s); off += 4 + le16(s, off+2) {
		if le16(s, off) == 0x0085 {
			pos := le32(s, off+4) + len(rec)
			copy(s[off+4:], le(pos))
		}
	}
	out := make([]byte, len(s))
	for off := 0; off < len(s); off += 1024 {
		end := min(off+1024, len(s))
		c, _ := rc4.NewCipher(key(uint32(off / 1024)))
		c.XORKeyStream(out[off:end], s[off:end])
	}
	for off := 0; off+4 <= len(s); off += 4 + le16(s, off+2) {
		typ, n := le16(s, off), le16(s, off+2)
		copy(out[off:off+4], s[off:off+4])
		switch {
		case typ == 0x0809 || typ == 0x002F:
			copy(out[off+4:off+4+n], s[off+4:off+4+n])
		case typ == 0x0085:
			copy(out[off+4:off+8], s[off+4:off+8])
		}
	}
	return out
}
