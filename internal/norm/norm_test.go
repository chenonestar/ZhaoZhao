package norm

import "testing"

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
