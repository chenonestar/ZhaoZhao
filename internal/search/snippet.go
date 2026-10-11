package search

import (
	"sort"
	"strconv"

	"github.com/chenonestar/zhaozhao/internal/store"
)

// Snippet 是一处命中的上下文片段。Marks 是片段内需要高亮的字符区间。
type Snippet struct {
	Text  string   `json:"text"`
	Marks [][2]int `json:"marks"`
	Loc   string   `json:"loc,omitempty"`
}

const snippetCtx = 40

func sortSpans(s []Span) []Span {
	sort.Slice(s, func(i, j int) bool {
		if s[i].S != s[j].S {
			return s[i].S < s[j].S
		}
		return s[i].E < s[j].E
	})
	return s
}

// mergeSpans 排序并合并重叠的区间。
func mergeSpans(s []Span) []Span {
	if len(s) == 0 {
		return nil
	}
	s = sortSpans(append([]Span(nil), s...))
	out := []Span{s[0]}
	for _, x := range s[1:] {
		l := &out[len(out)-1]
		if x.S < l.E {
			if x.E > l.E {
				l.E = x.E
			}
		} else {
			out = append(out, x)
		}
	}
	return out
}

// locator 把字符偏移换算为「第 3 页」「Sheet1!B2」「第 12 行」。
type locator struct {
	t     *store.Text
	rs    []rune
	lines []int // 每行起始偏移（按需构建）
}

func (l *locator) label(off int) string {
	if l.t == nil {
		return ""
	}
	if len(l.t.Locs) > 0 {
		i := sort.Search(len(l.t.Locs), func(i int) bool { return l.t.Locs[i].Off > off }) - 1
		if i >= 0 {
			return l.t.Locs[i].Label
		}
		return ""
	}
	if l.t.LineBased {
		if l.lines == nil {
			l.lines = []int{0}
			for i, r := range l.rs {
				if r == '\n' {
					l.lines = append(l.lines, i+1)
				}
			}
		}
		i := sort.Search(len(l.lines), func(i int) bool { return l.lines[i] > off }) - 1
		return "第" + strconv.Itoa(i+1) + "行"
	}
	return ""
}

// makeSnippets 为前 n 个互不重叠的命中生成片段。
func makeSnippets(rs []rune, t *store.Text, spans []Span, n int) []Snippet {
	spans = mergeSpans(spans)
	loc := &locator{t: t, rs: rs}
	var out []Snippet
	lastEnd := -1
	for _, sp := range spans {
		if len(out) >= n {
			break
		}
		if sp.S < lastEnd {
			continue
		}
		s := sp.S - snippetCtx
		if s < 0 {
			s = 0
		}
		e := sp.E + snippetCtx
		if e > len(rs) {
			e = len(rs)
		}
		if sp.E-sp.S > 200 { // 超长命中只高亮前部分
			e = sp.S + 200 + snippetCtx
			if e > len(rs) {
				e = len(rs)
			}
		}
		lastEnd = e
		seg := make([]rune, e-s)
		for i := s; i < e; i++ {
			r := rs[i]
			if r == '\n' || r == '\r' || r == '\t' {
				r = ' '
			}
			seg[i-s] = r
		}
		sn := Snippet{Text: string(seg), Loc: loc.label(sp.S)}
		for _, m := range spans {
			if m.E <= s || m.S >= e {
				continue
			}
			ms, me := m.S-s, m.E-s
			if ms < 0 {
				ms = 0
			}
			if me > len(seg) {
				me = len(seg)
			}
			sn.Marks = append(sn.Marks, [2]int{ms, me})
		}
		out = append(out, sn)
	}
	return out
}
