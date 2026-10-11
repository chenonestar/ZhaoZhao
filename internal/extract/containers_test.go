package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func writeZip(t *testing.T, p string, files map[string][]byte, gbkNames bool) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if gbkNames {
			g, _ := simplifiedchinese.GBK.NewEncoder().String(name)
			h.Name, h.NonUTF8 = g, true
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func childByInner(r doc.Result, inner string) *doc.Child {
	for i := range r.Children {
		if r.Children[i].Inner == inner {
			return &r.Children[i]
		}
	}
	return nil
}

func innerNames(r doc.Result) []string {
	var out []string
	for _, c := range r.Children {
		out = append(out, c.Inner)
	}
	return out
}

func TestZipBasicsAndGBKNames(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "2024.zip")
	writeZip(t, p, map[string][]byte{
		"合同/采购合同.txt": []byte("采购合同内容：违约金"),
		"说明.md":       []byte("# 说明\n里面有差旅费"),
		"data.bin":    {0, 1, 2, 3, 0, 0},
		"a.exe":       []byte("MZ"),
	}, true)
	r := Extract(p, "zip", DefaultOptions())
	if r.Status != doc.StatusOK || len(r.Children) != 4 {
		t.Fatalf("%+v", r)
	}
	c := childByInner(r, "合同/采购合同.txt")
	if c == nil || !strings.Contains(c.Text, "违约金") {
		t.Fatalf("GBK 文件名应被识别: %v", innerNames(r))
	}
	if c := childByInner(r, "data.bin"); c == nil || c.Status != doc.StatusSkipped {
		t.Fatalf("二进制文件只保留文件名: %+v", c)
	}
	// 不加密/UTF-8 文件名同样正常
	writeZip(t, p, map[string][]byte{"中文.txt": []byte("utf8 名字 内容")}, false)
	r = Extract(p, "zip", DefaultOptions())
	if c := childByInner(r, "中文.txt"); c == nil || !strings.Contains(c.Text, "内容") {
		t.Fatalf("%v", innerNames(r))
	}
}

func TestZipNestedAndDepth(t *testing.T) {
	d := t.TempDir()
	innermost := filepath.Join(d, "c.zip")
	writeZip(t, innermost, map[string][]byte{"deep.txt": []byte("最深层的文件 alpaca")}, false)
	b3, _ := os.ReadFile(innermost)
	mid := filepath.Join(d, "b.zip")
	writeZip(t, mid, map[string][]byte{"c.zip": b3, "mid.txt": []byte("中间层 llama")}, false)
	b2, _ := os.ReadFile(mid)
	outer := filepath.Join(d, "a.zip")
	writeZip(t, outer, map[string][]byte{"b.zip": b2, "top.txt": []byte("顶层 vicuna")}, false)

	opt := DefaultOptions()
	r := Extract(outer, "zip", opt)
	for inner, want := range map[string]string{
		"top.txt":                  "vicuna",
		"b.zip › mid.txt":          "llama",
		"b.zip › c.zip › deep.txt": "alpaca",
	} {
		c := childByInner(r, inner)
		if c == nil || !strings.Contains(c.Text, want) {
			t.Errorf("缺少 %q: %v", inner, innerNames(r))
		}
	}
	// 嵌套层数上限：默认 3 层，设为 2 层时最深的文件不再展开
	opt.MaxDepth = 2
	r = Extract(outer, "zip", opt)
	if childByInner(r, "b.zip › c.zip › deep.txt") != nil {
		t.Error("超过嵌套层数上限应不再展开")
	}
	if childByInner(r, "b.zip › c.zip") == nil {
		t.Error("容器本身的文件名仍应保留")
	}
}

func TestZipBombRatioAndBudget(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "bomb.zip")
	writeZip(t, p, map[string][]byte{"zeros.txt": make([]byte, 20<<20), "ok.txt": []byte("正常文件 zebra")}, false)
	r := Extract(p, "zip", DefaultOptions())
	if c := childByInner(r, "zeros.txt"); c == nil || c.Status != doc.StatusFailed || !strings.Contains(c.Err, "压缩比") {
		t.Fatalf("压缩比超限应跳过并记为失败: %+v", c)
	}
	if c := childByInner(r, "ok.txt"); c == nil || !strings.Contains(c.Text, "zebra") {
		t.Fatal("其他文件不受影响")
	}
	// 解压总量上限
	opt := DefaultOptions()
	opt.ArchiveMaxBytes = 10
	opt.ArchiveMaxRatio = 0
	r = Extract(p, "zip", opt)
	if c := childByInner(r, "ok.txt"); c == nil || c.Status == doc.StatusOK {
		t.Fatalf("超过解压总量上限应不再提取: %+v", c)
	}
}

func TestTarGz(t *testing.T) {
	d := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range map[string]string{"docs/a.txt": "tar 里的内容 ocelot", "b.md": "第二个 serval"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write([]byte(data))
	}
	tw.Close()
	gz.Close()
	for _, name := range []string{"x.tar.gz", "x.tgz"} {
		p := filepath.Join(d, name)
		os.WriteFile(p, buf.Bytes(), 0o644)
		r := Extract(p, Ext(name), DefaultOptions())
		c := childByInner(r, "docs/a.txt")
		if c == nil || !strings.Contains(c.Text, "ocelot") || childByInner(r, "b.md") == nil {
			t.Errorf("%s: %v %+v", name, innerNames(r), r)
		}
	}
	// 单个 .gz
	var gbuf bytes.Buffer
	g := gzip.NewWriter(&gbuf)
	g.Name = "app.log"
	g.Write([]byte("日志内容 caracal"))
	g.Close()
	p := filepath.Join(d, "app.log.gz")
	os.WriteFile(p, gbuf.Bytes(), 0o644)
	r := Extract(p, "gz", DefaultOptions())
	if c := childByInner(r, "app.log"); c == nil || !strings.Contains(c.Text, "caracal") {
		t.Fatalf("%+v", r)
	}
}

const emlSample = "From: =?UTF-8?B?5byg5LiJ?= <zhangsan@example.com>\r\n" +
	"To: lisi@example.com\r\n" +
	"Subject: =?UTF-8?B?5YWz5LqO5oql6ZSA55qE6YCa55+l?=\r\n" +
	"Date: Mon, 07 Jul 2025 10:30:00 +0800\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; charset=gb2312\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"%GBKBODY%\r\n" +
	"--BOUND\r\n" +
	"Content-Type: text/plain; name=\"=?UTF-8?B?6ZmE5Lu2LnR4dA==?=\"\r\n" +
	"Content-Disposition: attachment; filename=\"=?UTF-8?B?6ZmE5Lu2LnR4dA==?=\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"%ATT%\r\n" +
	"--BOUND--\r\n"

func makeEML(t *testing.T) []byte {
	gbk, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("各位好，请于本周五前提交差旅费报销单据。"))
	b64 := func(b []byte) string {
		const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		var sb strings.Builder
		for i := 0; i < len(b); i += 3 {
			var v uint32
			n := 0
			for j := 0; j < 3; j++ {
				v <<= 8
				if i+j < len(b) {
					v |= uint32(b[i+j])
					n++
				}
			}
			for k := 0; k < 4; k++ {
				if k <= n {
					sb.WriteByte(tbl[(v>>(18-6*uint(k)))&63])
				} else {
					sb.WriteByte('=')
				}
			}
		}
		return sb.String()
	}
	s := strings.ReplaceAll(emlSample, "%GBKBODY%", b64(gbk))
	s = strings.ReplaceAll(s, "%ATT%", b64([]byte("附件里的文字：报销清单 pangolin")))
	return []byte(s)
}

func TestEML(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "通知.eml")
	os.WriteFile(p, makeEML(t), 0o644)
	r := Extract(p, "eml", DefaultOptions())
	if r.Status != doc.StatusOK || !strings.Contains(r.Text, "差旅费报销单据") || !strings.Contains(r.Text, "关于报销的通知") {
		t.Fatalf("正文（GB2312 + base64）和主题（RFC2047）都应解出: %+v", r)
	}
	if !strings.Contains(r.Meta, "张三") || !strings.Contains(r.Meta, "2025-07-07") {
		t.Fatalf("meta: %s", r.Meta)
	}
	c := childByInner(r, "附件.txt")
	if c == nil || !strings.Contains(c.Text, "pangolin") {
		t.Fatalf("附件应作为独立结果: %v", innerNames(r))
	}
	// 解出附件用于打开
	out, err := ExtractInner(p, "附件.txt", d, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "pangolin") {
		t.Fatalf("%s: %q", out, b)
	}
}

func TestMbox(t *testing.T) {
	d := t.TempDir()
	eml := string(makeEML(t))
	second := "From: b@example.com\r\nTo: c@example.com\r\nSubject: second mail\r\nDate: Tue, 08 Jul 2025 09:00:00 +0000\r\n\r\nHello numbat\r\n>From the beginning, this line was escaped.\r\n"
	mbox := "From zhangsan@example.com Mon Jul  7 10:30:00 2025\n" + strings.ReplaceAll(eml, "\r\n", "\n") + "\n" +
		"From b@example.com Tue Jul  8 09:00:00 2025\n" + strings.ReplaceAll(second, "\r\n", "\n")
	p := filepath.Join(d, "收件箱.mbox")
	os.WriteFile(p, []byte(mbox), 0o644)
	r := Extract(p, "mbox", DefaultOptions())
	if r.Status != doc.StatusOK {
		t.Fatalf("%+v", r)
	}
	m1 := childByInner(r, "关于报销的通知")
	m2 := childByInner(r, "second mail")
	att := childByInner(r, "关于报销的通知 › 附件.txt")
	if m1 == nil || m2 == nil || att == nil {
		t.Fatalf("%v", innerNames(r))
	}
	if !strings.Contains(m1.Text, "差旅费") || !strings.Contains(m2.Text, "numbat") || !strings.Contains(m2.Text, "From the beginning") {
		t.Fatalf("%q / %q", m1.Text, m2.Text)
	}
	if !strings.Contains(att.Text, "pangolin") || m1.MTime == 0 || !strings.Contains(m1.Meta, "subject") {
		t.Fatalf("附件=%+v 邮件 mtime=%d meta=%s", att, m1.MTime, m1.Meta)
	}
	out, err := ExtractInner(p, "关于报销的通知 › 附件.txt", d, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "pangolin") {
		t.Fatalf("%q", b)
	}
}

func TestExtractInnerZipNested(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.zip")
	writeZip(t, in, map[string][]byte{"x/y.txt": []byte("嵌套文件 quokka")}, false)
	b, _ := os.ReadFile(in)
	outer := filepath.Join(d, "out.zip")
	writeZip(t, outer, map[string][]byte{"in.zip": b}, false)
	dest := t.TempDir()
	p, err := ExtractInner(outer, "in.zip › x/y.txt", dest, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "quokka") || filepath.Base(p) != "y.txt" {
		t.Fatalf("%s %q", p, got)
	}
	if _, err := ExtractInner(outer, "不存在", dest, DefaultOptions()); err == nil {
		t.Fatal("找不到应报错")
	}
}

func TestOfficeInsideZip(t *testing.T) {
	d := fixtures(t)
	docx, err := os.ReadFile(filepath.Join(d, "合同.docx"))
	if err != nil {
		t.Skip("无样本")
	}
	p := filepath.Join(t.TempDir(), "归档.zip")
	writeZip(t, p, map[string][]byte{"合同/采购合同.docx": docx}, true)
	r := Extract(p, "zip", DefaultOptions())
	c := childByInner(r, "合同/采购合同.docx")
	if c == nil || !strings.Contains(c.Text, "违约金") || len(c.Locs) == 0 {
		t.Fatalf("压缩包内的 docx 应被提取并带位置: %+v", c)
	}
}

func TestSevenZip(t *testing.T) {
	p := os.Getenv("ZHAOZHAO_FIXTURES")
	if p == "" {
		t.Skip("未设置 ZHAOZHAO_FIXTURES")
	}
	p7 := filepath.Join(p, "归档.7z")
	if _, err := os.Stat(p7); err != nil {
		t.Skip("没有 7z 样本")
	}
	r := Extract(p7, "7z", DefaultOptions())
	t.Logf("%+v", r)
	if c := childByInner(r, "合同/说明.txt"); c == nil || !strings.Contains(c.Text, "海獭") {
		t.Fatalf("%v", innerNames(r))
	}
	enc := Extract(filepath.Join(p, "加密.7z"), "7z", DefaultOptions())
	if enc.Status != doc.StatusEncrypted {
		t.Fatalf("加密 7z 应标记已加密: %+v", enc)
	}
}
