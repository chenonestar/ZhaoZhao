package extract

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func randB64(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// 文字里夹着的编码数据（嵌入的照片等）去掉，正常文字、行号不变；不像编码数据的长串保留。
func TestDropEncoded(t *testing.T) {
	photo := randB64(30000)
	in := "姓名 张三\n<ZhaoPian>" + photo + "</ZhaoPian>\n简历 1990 年参加工作"
	got := dropEncoded(in, false)
	if strings.Contains(got, photo[:50]) || !strings.Contains(got, "姓名 张三") || !strings.Contains(got, "简历 1990 年参加工作") ||
		strings.Count(got, "\n") != 2 {
		t.Fatalf("一行里的照片编码应去掉、其余不变：%q", got[:min(len(got), 200)])
	}
	// 折行的 base64（每行 76 个字符，邮件、PEM 常见）：去掉内容，保留换行
	var wrapped strings.Builder
	enc := randB64(3000)
	for i := 0; i < len(enc); i += 76 {
		wrapped.WriteString("  " + enc[i:min(i+76, len(enc))] + "\n")
	}
	in = "开头\n" + wrapped.String() + "结尾"
	got = dropEncoded(in, false)
	if strings.Contains(got, enc[:40]) || !strings.Contains(got, "开头") || !strings.Contains(got, "结尾") || strings.Count(got, "\n") != strings.Count(in, "\n") {
		t.Fatalf("折行的编码应去掉、行数不变：%q", got)
	}
	// 十六进制
	hb := make([]byte, 200)
	rand.Read(hb)
	hx := hex.EncodeToString(hb)
	if got := dropEncoded("图片数据 "+hx+" 结束", false); strings.Contains(got, hx[:40]) {
		t.Fatal("长串十六进制应去掉")
	}
	// 位置不变：换成空格
	in = "前面" + photo + "后面"
	if got := dropEncoded(in, true); len([]rune(got)) != len([]rune(in)) || !strings.HasSuffix(got, "后面") || strings.Contains(got, photo[:50]) {
		t.Fatal("keepLen 时应换成等长的空格")
	}
	// 保留：正常的长句子、长串数字、只有字母、文件指纹、分隔线、短的编码
	for _, keep := range []string{
		strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20),
		strings.Repeat("1234567890", 30),
		strings.Repeat("abcdefghij", 30),
		"sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		strings.Repeat("-", 300) + "\n" + strings.Repeat("-", 300) + "\n" + strings.Repeat("-", 300),
		"token " + randB64(100),
		"https://example.com/a/b/c?x=" + strings.Repeat("ab12", 30) + "&y=1",
	} {
		if got := dropEncoded(keep, false); got != keep {
			t.Fatalf("不应去掉：%q → %q", keep[:min(len(keep), 80)], got[:min(len(got), 80)])
		}
	}
}

// 任免审批表（.lrmx）当 XML 取文字：元素文字、属性值、CDATA 每段一行，去掉标签、注释和照片编码。
func TestLrmxText(t *testing.T) {
	photo := randB64(300000)
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<!-- 由任免审批表软件生成 -->
<Person xmlns="urn:test" xmlns:x="urn:x">
  <XingMing>张三</XingMing>
  <XingBie>男</XingBie>
  <ZhaoPian>` + photo + `</ZhaoPian>
  <JianLi><![CDATA[1990.07--1995.08 某某大学学习 & 工作]]></JianLi>
  <Item ChengWei="妻子" XingMing="李四" />
  <BeiZhu>收入 &lt; 1 万 &amp; 其他</BeiZhu>
</Person>`
	dir := t.TempDir()
	p := filepath.Join(dir, "任免审批表.lrmx")
	if err := os.WriteFile(p, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Extract(p, "lrmx", DefaultOptions())
	want := "张三\n男\n1990.07--1995.08 某某大学学习 & 工作\n妻子\n李四\n收入 < 1 万 & 其他"
	if r.Status != "ok" || r.Text != want || !r.LineBased {
		t.Fatalf("lrmx 提取结果：%v %q", r.Status, r.Text[:min(len(r.Text), 300)])
	}
	if k := KindOf("lrmx"); k != KindDoc {
		t.Fatalf("lrmx 应算文档：%s", k)
	}
}
