package extract

import "testing"

func TestJoinWords(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"中", "华", "人民", "共和国"}, "中华人民共和国"},
		{[]string{"Hello", "world"}, "Hello world"},
		{[]string{"共", "3", "页", "PDF", "文件", "，", "2024", "年"}, "共3页PDF文件，2024年"},
		{[]string{"第", "12", "条"}, "第12条"},
		{nil, ""},
	} {
		if got := joinWords(c.in); got != c.want {
			t.Errorf("joinWords(%q) = %q，应为 %q", c.in, got, c.want)
		}
	}
}
