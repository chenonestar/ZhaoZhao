package norm

import (
	"math/rand"
	"strings"
	"testing"
)

func TestRuneFold(t *testing.T) {
	o := Options{Simplify: true}
	cases := map[string]string{
		"ＡＢＣ１２３":      "abc123",
		"合同，审批":       "合同,审批",
		"合約審批":        "合约审批",
		"Hello World": "hello world",
		"（甲方）":        "(甲方)",
	}
	for in, want := range cases {
		if got := o.String(in); got != want {
			t.Errorf("%q => %q, want %q", in, got, want)
		}
		if len([]rune(o.String(in))) != len([]rune(in)) {
			t.Errorf("length changed for %q", in)
		}
	}
	if (Options{}).String("合約") != "合约"[:0]+"合約" {
		t.Error("simplify disabled should keep traditional")
	}
}

func TestTokenize(t *testing.T) {
	toks := Tokenize([]rune("张三 report2025_v2,合同"))
	var got []string
	for _, k := range toks {
		got = append(got, k.Text)
	}
	want := []string{"张", "三", "report2025", "v2", "合", "同"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v != %v", got, want)
		}
	}
	if FTSText(toks) != "张 三 report2025 v2 合 同" {
		t.Fatal(FTSText(toks))
	}
}

// FTS 与「归一化 → 分词 → 拼接」三步的结果必须完全相同（已入库的文本是按后者生成的）。
func TestFTSMatchesTokenize(t *testing.T) {
	pieces := []string{"合同", "審批", "Report", "ＡＢＣ１２３", "v2.0", "_", " ", "，", "。", "（甲方）", "ΣΑΣ", "Straße",
		"ひらがな", "カタカナ", "ｶﾀｶﾅ", "한국어", "가", "㐀", "𠀀", "\xff\xfe", "　", "々", "123", "x\ty", "Ⅻ", "é"}
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := rnd.Intn(12); n > 0; n-- {
			b.WriteString(pieces[rnd.Intn(len(pieces))])
		}
		s := b.String()
		for _, o := range []Options{{}, {Simplify: true}} {
			if got, want := o.FTS(s), FTSText(Tokenize(o.Runes(s))); got != want {
				t.Fatalf("%q（繁简 %v）：%q，应为 %q", s, o.Simplify, got, want)
			}
		}
	}
	// 每个字符都要过一遍（含所有常用汉字、繁体字、全角字符、各种文字的大小写）
	var all strings.Builder
	for r := rune(0); r < 0x30000; r += 1 {
		if r < 0xD800 || r > 0xDFFF {
			all.WriteRune(r)
		}
	}
	for _, o := range []Options{{}, {Simplify: true}} {
		if o.FTS(all.String()) != FTSText(Tokenize(o.Runes(all.String()))) {
			t.Fatalf("全部字符（繁简 %v）结果不同", o.Simplify)
		}
	}
}

func BenchmarkFTS(b *testing.B) {
	s := strings.Repeat("企业人工成本情况统计报表 Report 2025，法人单位名称。", 20000) // 约 1.3 MB
	b.Run("一次遍历", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = Options{}.FTS(s)
		}
	})
	b.Run("三步", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = FTSText(Tokenize(Options{}.Runes(s)))
		}
	})
}
