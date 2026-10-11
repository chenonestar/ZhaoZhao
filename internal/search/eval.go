package search

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/query"
)

// Span 是命中的字符区间（左闭右开）。
type Span struct{ S, E int }

const maxSpansPerDoc = 5000

// docView 是一个待校验文档的归一化视图。
type docView struct {
	nameRunes []rune
	nameToks  []norm.Token
	name      string

	text     string // 原文
	runes    []rune // 归一化后的内容（与原文字符一一对应）
	toks     []norm.Token
	hasText  bool
	fuzzyHit bool
}

func newDocView(o norm.Options, name, text string, hasText bool) *docView {
	d := &docView{name: name, text: text, hasText: hasText}
	d.nameRunes = o.Runes(name)
	d.nameToks = norm.Tokenize(d.nameRunes)
	if hasText {
		d.runes = o.Runes(text)
		d.toks = norm.Tokenize(d.runes)
	}
	return d
}

type evaluator struct {
	o         norm.Options
	scope     string
	caseSens  bool
	fuzzyAll  bool
	d         *docView
	cspans    []Span // 内容命中
	nspans    []Span // 文件名命中
	meta      *metaEval
	regexes   map[string]*regexp.Regexp
	collect   bool
	fuzzyOnly bool // 命中全部来自模糊匹配
	sawExact  bool
	sawFuzzy  bool
}

func (e *evaluator) regex(expr string) *regexp.Regexp {
	key := expr
	if re, ok := e.regexes[key]; ok {
		return re
	}
	pat := expr
	if !e.caseSens {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		re = nil
	}
	e.regexes[key] = re
	return re
}

func (e *evaluator) eval(n query.Node) bool {
	switch t := n.(type) {
	case *query.And:
		ok := true
		for _, c := range t.Items {
			if !e.eval(c) {
				ok = false
			}
		}
		return ok
	case *query.Or:
		ok := false
		for _, c := range t.Items {
			if e.eval(c) {
				ok = true
			}
		}
		return ok
	case *query.Not:
		c, nn, ef, sf, se := len(e.cspans), len(e.nspans), e.sawFuzzy, e.sawExact, e.collect
		e.collect = false
		r := e.eval(t.Item)
		e.collect = se
		e.cspans, e.nspans, e.sawFuzzy, e.sawExact = e.cspans[:c], e.nspans[:nn], ef, sf
		return !r
	case *query.Filter:
		return e.meta.filter(t)
	case *query.Term:
		cs, ns, fz := e.matchTerm(t)
		return e.record(cs, ns, fz)
	case *query.Near:
		return e.matchNear(t)
	case *query.Regex:
		return e.matchRegex(t)
	}
	return false
}

func (e *evaluator) record(cs, ns []Span, fuzzy bool) bool {
	if len(cs)+len(ns) == 0 {
		return false
	}
	if e.collect {
		e.cspans = append(e.cspans, cs...)
		e.nspans = append(e.nspans, ns...)
		if fuzzy {
			e.sawFuzzy = true
		} else {
			e.sawExact = true
		}
	}
	return true
}

func (e *evaluator) inContent(nameOnly bool) bool {
	return !nameOnly && e.scope != "name" && e.d.hasText
}
func (e *evaluator) inName(nameOnly bool) bool { return nameOnly || e.scope != "content" }

// matchTerm 在内容和文件名里查找一个词。
func (e *evaluator) matchTerm(t *query.Term) (cs, ns []Span, fuzzy bool) {
	q := norm.Tokenize(e.o.Runes(t.Text))
	useFuzzy := t.Fuzzy || (e.fuzzyAll && !t.Phrase)
	if len(q) == 0 {
		// 纯符号：按归一化后的字符子串匹配
		qr := e.o.Runes(strings.TrimSpace(t.Text))
		if len(qr) == 0 {
			return
		}
		if e.inContent(t.NameOnly) {
			cs = substrSpans(e.d.runes, qr)
		}
		if e.inName(t.NameOnly) {
			ns = substrSpans(e.d.nameRunes, qr)
		}
		return
	}
	if e.inContent(t.NameOnly) {
		cs, fuzzy = findTerm(e.d.toks, q, useFuzzy)
	}
	if e.inName(t.NameOnly) {
		var f bool
		ns, f = findTerm(e.d.nameToks, q, useFuzzy)
		fuzzy = fuzzy || f
	}
	return
}

func substrSpans(hay, needle []rune) []Span {
	var out []Span
	for i := 0; i+len(needle) <= len(hay) && len(out) < maxSpansPerDoc; i++ {
		ok := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, Span{i, i + len(needle)})
			i += len(needle) - 1
		}
	}
	return out
}

// findTerm 在词元序列里找词/短语。精确匹配优先；模糊词在没有精确命中时才启用模糊。
func findTerm(dt, q []norm.Token, fuzzy bool) ([]Span, bool) {
	spans := findPhrase(dt, q)
	if len(spans) > 0 || !fuzzy {
		return spans, false
	}
	switch {
	case len(q) >= 2 && allCJK(q):
		return findProximity(dt, q, 2), true
	case len(q) == 1 && !q[0].CJK:
		k := editBudget(utf8.RuneCountInString(q[0].Text))
		if k == 0 {
			return nil, false
		}
		var out []Span
		for _, t := range dt {
			if !t.CJK && withinEdit(q[0].Text, t.Text, k) {
				out = append(out, Span{t.Start, t.End})
				if len(out) >= maxSpansPerDoc {
					break
				}
			}
		}
		return out, true
	}
	return nil, false
}

// findPhrase 查找连续词元序列。除最后一个词元外必须整词相等；
// 最后一个英文/数字词元按前缀匹配。标点和空白不影响相邻判断。
func findPhrase(dt, q []norm.Token) []Span {
	var out []Span
	m := len(q)
	last := q[m-1]
	for i := 0; i+m <= len(dt); i++ {
		ok := true
		for k := 0; k < m; k++ {
			d := dt[i+k]
			if k == m-1 && !last.CJK {
				if d.CJK || !strings.HasPrefix(d.Text, last.Text) {
					ok = false
				}
			} else if d.Text != q[k].Text {
				ok = false
			}
			if !ok {
				break
			}
		}
		if ok {
			out = append(out, Span{dt[i].Start, dt[i+m-1].End})
			if len(out) >= maxSpansPerDoc {
				break
			}
		}
	}
	return out
}

// findProximity 中文邻近匹配：查询词的字按原顺序出现，相邻两字之间最多夹 gap 个字。
func findProximity(dt, q []norm.Token, gap int) []Span {
	var out []Span
	for i := 0; i < len(dt); i++ {
		if dt[i].Text != q[0].Text {
			continue
		}
		pos := i
		ok := true
		for k := 1; k < len(q); k++ {
			found := -1
			for j := pos + 1; j < len(dt) && j <= pos+1+gap; j++ {
				if dt[j].Text == q[k].Text {
					found = j
					break
				}
			}
			if found < 0 {
				ok = false
				break
			}
			pos = found
		}
		if ok {
			out = append(out, Span{dt[i].Start, dt[pos].End})
			if len(out) >= maxSpansPerDoc {
				break
			}
		}
	}
	return out
}

func (e *evaluator) matchNear(n *query.Near) bool {
	ac, an, af := e.matchTerm(n.A)
	bc, bn, bf := e.matchTerm(n.B)
	var hitsC, hitsN []Span
	near := func(a, b []Span) (ra, rb []Span) {
		for _, x := range a {
			for _, y := range b {
				d := 0
				if y.S >= x.E {
					d = y.S - x.E
				} else if x.S >= y.E {
					d = x.S - y.E
				} // 重叠视为距离 0
				if d <= n.N {
					ra = append(ra, x)
					rb = append(rb, y)
				}
			}
		}
		return
	}
	xa, xb := near(ac, bc)
	hitsC = append(xa, xb...)
	ya, yb := near(an, bn)
	hitsN = append(ya, yb...)
	return e.record(dedupSpans(hitsC), dedupSpans(hitsN), af || bf)
}

func dedupSpans(s []Span) []Span {
	if len(s) < 2 {
		return s
	}
	seen := map[Span]bool{}
	out := s[:0:0]
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return sortSpans(out)
}

func (e *evaluator) matchRegex(r *query.Regex) bool {
	re := e.regex(r.Expr)
	if re == nil {
		return false
	}
	var cs, ns []Span
	if e.inContent(false) {
		cs = regexSpans(re, e.d.text)
	}
	if e.inName(false) {
		ns = regexSpans(re, e.d.name)
	}
	return e.record(cs, ns, false)
}

// regexSpans 返回正则的全部非空匹配，区间以字符偏移表示。
func regexSpans(re *regexp.Regexp, s string) []Span {
	idx := re.FindAllStringIndex(s, maxSpansPerDoc)
	if len(idx) == 0 {
		return nil
	}
	out := make([]Span, 0, len(idx))
	// 字节偏移 → 字符偏移，单次线性扫描
	rpos, bpos := 0, 0
	conv := func(b int) int {
		for bpos < b {
			_, w := utf8.DecodeRuneInString(s[bpos:])
			bpos += w
			rpos++
		}
		return rpos
	}
	for _, m := range idx {
		if m[1] <= m[0] {
			continue
		}
		st := conv(m[0])
		en := conv(m[1])
		out = append(out, Span{st, en})
	}
	return out
}
