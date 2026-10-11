// Package search 执行查询：用 SQLite FTS5 预筛候选，再用 Go 精确校验、定位命中并生成片段。
package search

import (
	"fmt"
	"regexp/syntax"
	"strings"

	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/query"
	"github.com/chenonestar/zhaozhao/internal/store"
)

type fxKind int

const (
	fxAll  fxKind = iota // 不能用索引限定，所有文件都是候选
	fxNone               // 肯定没有结果
	fxExpr               // 有 FTS5 表达式
)

// fx 是子树翻译成 FTS5 表达式的结果。exact 表示索引的命中集合与查询语义完全一致，
// 不必再逐个文件精确校验；否则表达式只是候选的超集。
type fx struct {
	kind  fxKind
	s     string
	exact bool
}

type planner struct {
	db       *store.DB
	norm     norm.Options
	scope    string // all | content | name
	fuzzyAll bool   // 全局模糊开关
}

func (p *planner) col(nameOnly bool) string {
	switch {
	case nameOnly:
		return "n : "
	case p.scope == "content":
		return "c : "
	case p.scope == "name":
		return "n : "
	}
	return ""
}

func quote(tok string) string { return `"` + strings.ReplaceAll(tok, `"`, ``) + `"` }

// phraseExpr 构造短语表达式。规则：短语中除最后一个词元外必须整词相等，
// 最后一个英文/数字词元按前缀匹配（支持边输入边搜）。
func phraseExpr(toks []norm.Token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.Text
	}
	e := `"` + strings.Join(parts, " ") + `"`
	if !toks[len(toks)-1].CJK {
		e += " *"
	}
	return e
}

func (p *planner) plan(n query.Node) fx {
	switch t := n.(type) {
	case *query.Term:
		return p.planTerm(t)
	case *query.Near:
		a, b := p.planTerm(t.A), p.planTerm(t.B)
		return andFx([]fx{a, b}, false)
	case *query.Regex:
		return p.planRegex(t)
	case *query.Filter:
		return fx{kind: fxAll, exact: true}
	case *query.And:
		return p.planAnd(t)
	case *query.Or:
		var parts []string
		exact := true
		for _, c := range t.Items {
			f := p.plan(c)
			switch f.kind {
			case fxAll:
				return fx{kind: fxAll}
			case fxNone:
				continue
			}
			parts = append(parts, "("+f.s+")")
			exact = exact && f.exact
		}
		if len(parts) == 0 {
			return fx{kind: fxNone}
		}
		return fx{kind: fxExpr, s: strings.Join(parts, " OR "), exact: exact}
	case *query.Not:
		return fx{kind: fxAll}
	}
	return fx{kind: fxAll}
}

func andFx(fs []fx, exact bool) fx {
	var parts []string
	for _, f := range fs {
		switch f.kind {
		case fxNone:
			return fx{kind: fxNone}
		case fxAll:
			continue
		}
		parts = append(parts, "("+f.s+")")
		exact = exact && f.exact
	}
	if len(parts) == 0 {
		return fx{kind: fxAll}
	}
	return fx{kind: fxExpr, s: strings.Join(parts, " AND "), exact: exact}
}

func (p *planner) planAnd(a *query.And) fx {
	var pos []fx
	var neg []string
	exact := true
	for _, c := range a.Items {
		if n, ok := c.(*query.Not); ok {
			f := p.plan(n.Item)
			switch {
			case f.kind == fxExpr && f.exact:
				neg = append(neg, "("+f.s+")")
			case f.kind == fxNone:
				// NOT 一个必然不存在的东西：恒真，无需处理
			default:
				exact = false // 需要逐个校验
			}
			continue
		}
		f := p.plan(c)
		pos = append(pos, f)
		if f.kind == fxAll && !f.exact {
			exact = false
		}
	}
	r := andFx(pos, exact)
	if r.kind == fxNone {
		return r
	}
	if len(neg) > 0 {
		if r.kind == fxAll {
			// 只有排除条件：无法用 FTS5 表达，退化为逐个校验
			return fx{kind: fxAll}
		}
		r.s = "(" + r.s + ") NOT (" + strings.Join(neg, " OR ") + ")"
	}
	if r.kind == fxAll {
		r.exact = exact && len(neg) == 0
	}
	return r
}

func (p *planner) planTerm(t *query.Term) fx {
	toks := norm.Tokenize(p.norm.Runes(t.Text))
	if len(toks) == 0 {
		return fx{kind: fxAll} // 纯符号：校验阶段按子串匹配
	}
	col := p.col(t.NameOnly)
	fuzzy := t.Fuzzy || (p.fuzzyAll && !t.Phrase)
	if fuzzy && !t.Phrase {
		if f, ok := p.planFuzzy(toks, col); ok {
			return f
		}
	}
	return fx{kind: fxExpr, s: col + phraseExpr(toks), exact: true}
}

func allCJK(toks []norm.Token) bool {
	for _, t := range toks {
		if !t.CJK {
			return false
		}
	}
	return true
}

// planFuzzy 返回模糊词的候选表达式（超集）。ok=false 表示该词不适用模糊，按精确处理。
func (p *planner) planFuzzy(toks []norm.Token, col string) (fx, bool) {
	switch {
	case len(toks) >= 2 && allCJK(toks):
		seen := map[string]bool{}
		var parts []string
		for _, t := range toks {
			if !seen[t.Text] {
				seen[t.Text] = true
				parts = append(parts, quote(t.Text))
			}
		}
		return fx{kind: fxExpr, s: col + "(" + strings.Join(parts, " AND ") + ")"}, true
	case len(toks) == 1 && !toks[0].CJK:
		k := editBudget(len([]rune(toks[0].Text)))
		if k == 0 {
			return fx{}, false
		}
		terms := p.vocabNear(toks[0].Text, k)
		parts := []string{quote(toks[0].Text) + " *"}
		for _, v := range terms {
			parts = append(parts, quote(v))
		}
		return fx{kind: fxExpr, s: col + "(" + strings.Join(parts, " OR ") + ")"}, true
	}
	return fx{}, false
}

// editBudget 是英文/数字词按长度允许的编辑距离：5–8 位允许 1，9 位以上允许 2。
func editBudget(n int) int {
	switch {
	case n >= 9:
		return 2
	case n >= 5:
		return 1
	}
	return 0
}

// vocabNear 在索引词表里找出与 w 编辑距离不超过 k 的词。
func (p *planner) vocabNear(w string, k int) []string {
	wl := len([]rune(w))
	rows, err := p.db.R.Query(`SELECT term FROM fts_v WHERE length(term) BETWEEN ? AND ?`, wl-k, wl+k)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) != nil {
			continue
		}
		if t != w && withinEdit(w, t, k) {
			out = append(out, t)
			if len(out) >= 400 {
				break
			}
		}
	}
	return out
}

func (p *planner) planRegex(r *query.Regex) fx {
	re, err := syntax.Parse(r.Expr, syntax.Perl)
	if err != nil {
		return fx{kind: fxAll}
	}
	var lits []string
	collectLiterals(re.Simplify(), &lits)
	col := p.col(false)
	var parts []string
	for _, l := range lits {
		rs := p.norm.Runes(l)
		var run []rune
		flush := func() {
			if len(run) == 0 {
				return
			}
			toks := norm.Tokenize(run)
			parts = append(parts, "("+col+phraseExpr(toks)+")")
			run = nil
		}
		for _, c := range rs {
			if norm.IsCJK(c) {
				run = append(run, c)
			} else {
				flush()
			}
		}
		flush()
	}
	if len(parts) == 0 {
		return fx{kind: fxAll}
	}
	return fx{kind: fxExpr, s: strings.Join(parts, " AND ")}
}

// collectLiterals 收集正则里「必然出现」的字面文字。
func collectLiterals(re *syntax.Regexp, out *[]string) {
	switch re.Op {
	case syntax.OpLiteral:
		*out = append(*out, string(re.Rune))
	case syntax.OpConcat:
		for _, s := range re.Sub {
			collectLiterals(s, out)
		}
	case syntax.OpCapture, syntax.OpPlus:
		collectLiterals(re.Sub[0], out)
	case syntax.OpRepeat:
		if re.Min >= 1 {
			collectLiterals(re.Sub[0], out)
		}
	}
}

// validate 检查正则和过滤器的写法，在执行前给出友好的错误。
func validate(n query.Node) error {
	switch t := n.(type) {
	case *query.And:
		for _, c := range t.Items {
			if err := validate(c); err != nil {
				return err
			}
		}
	case *query.Or:
		for _, c := range t.Items {
			if err := validate(c); err != nil {
				return err
			}
		}
	case *query.Not:
		return validate(t.Item)
	case *query.Regex:
		if _, err := syntax.Parse(t.Expr, syntax.Perl); err != nil {
			return fmt.Errorf("正则表达式有误（采用 RE2 语法，不支持反向引用和环视）：%v", err)
		}
	case *query.Filter:
		return checkFilter(t)
	}
	return nil
}
