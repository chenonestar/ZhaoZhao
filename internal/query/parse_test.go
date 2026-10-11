package query

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Node
	}{
		{"合同 违约金", &And{[]Node{&Term{Text: "合同"}, &Term{Text: "违约金"}}}},
		{`"不可抗力条款"`, &Term{Text: "不可抗力条款", Phrase: true}},
		{"报销 OR 差旅", &Or{[]Node{&Term{Text: "报销"}, &Term{Text: "差旅"}}}},
		{"张三 报销 -草稿", &And{[]Node{&Term{Text: "张三"}, &Term{Text: "报销"}, &Not{&Term{Text: "草稿"}}}}},
		{"张三 NOT 草稿", &And{[]Node{&Term{Text: "张三"}, &Not{&Term{Text: "草稿"}}}}},
		{"(合同 OR 协议) 2025", &And{[]Node{&Or{[]Node{&Term{Text: "合同"}, &Term{Text: "协议"}}}, &Term{Text: "2025"}}}},
		{"甲方 NEAR/10 违约", &Near{A: &Term{Text: "甲方"}, B: &Term{Text: "违约"}, N: 10}},
		{"合同审批~", &Term{Text: "合同审批", Fuzzy: true}},
		{`/1[3-9]\d{9}/`, &Regex{Expr: `1[3-9]\d{9}`}},
		{`path:D:\项目\2025`, &Filter{Key: "path", Value: `D:\项目\2025`}},
		{`path:"D:\我的 项目"`, &Filter{Key: "path", Value: `D:\我的 项目`}},
		{"name:周报", &Term{Text: "周报", NameOnly: true}},
		{"modified:2025-01..2025-06", &Filter{Key: "modified", Value: "2025-01..2025-06"}},
		{"a-b", &Term{Text: "a-b"}},
		{"OR", nil},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.want == nil {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %#v\nwant %#v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{`"abc`, "(a b", "a )", "a NEAR/x b", "/abc", "a NEAR/3"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	if n, err := Parse("   "); n != nil || err != nil {
		t.Error("empty query")
	}
}
