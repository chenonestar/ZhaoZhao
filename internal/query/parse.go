package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

var filterKeys = map[string]bool{
	"path": true, "ext": true, "type": true, "name": true, "from": true, "to": true,
	"subject": true, "modified": true, "size": true, "vol": true,
}

type tokKind int

const (
	tWord tokKind = iota
	tPhrase
	tRegex
	tFilter
	tLParen
	tRParen
	tOr
	tNot
	tMinus
	tNear
)

type tok struct {
	kind tokKind
	text string // 词/短语/正则内容，或过滤器的值
	key  string // 过滤器的键
	n    int    // NEAR/n 的 n
}

// Parse 把查询字符串解析为语法树。空查询返回 (nil, nil)。
func Parse(input string) (Node, error) {
	toks, err := lex(input)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, nil
	}
	p := &parser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("多余的 %s", p.describe())
	}
	return n, nil
}

func lex(s string) ([]tok, error) {
	rs := []rune(s)
	var out []tok
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			out = append(out, tok{kind: tLParen})
			i++
		case r == ')':
			out = append(out, tok{kind: tRParen})
			i++
		case r == '"':
			j, text, err := readQuoted(rs, i)
			if err != nil {
				return nil, err
			}
			i = j
			out = append(out, tok{kind: tPhrase, text: text})
		case r == '/':
			j, text, err := readRegex(rs, i)
			if err != nil {
				return nil, err
			}
			i = j
			out = append(out, tok{kind: tRegex, text: text})
		case r == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]):
			out = append(out, tok{kind: tMinus})
			i++
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && rs[j] != '(' && rs[j] != ')' && rs[j] != '"' {
				j++
			}
			word := string(rs[i:j])
			// key:value，值可以是引号或正则
			if k := strings.IndexRune(word, ':'); k > 0 && filterKeys[strings.ToLower(word[:k])] {
				key := strings.ToLower(word[:k])
				if k == len(word)-1 && j < len(rs) && rs[j] == '"' {
					e, text, err := readQuoted(rs, j)
					if err != nil {
						return nil, err
					}
					out = append(out, tok{kind: tFilter, key: key, text: text})
					i = e
					continue
				}
				out = append(out, tok{kind: tFilter, key: key, text: word[k+1:]})
				i = j
				continue
			}
			i = j
			switch {
			case word == "OR":
				out = append(out, tok{kind: tOr})
			case word == "NOT":
				out = append(out, tok{kind: tNot})
			case strings.HasPrefix(word, "NEAR/"):
				n, err := strconv.Atoi(word[5:])
				if err != nil || n < 0 {
					return nil, fmt.Errorf("NEAR/n 中的 n 必须是非负整数：%s", word)
				}
				out = append(out, tok{kind: tNear, n: n})
			default:
				out = append(out, tok{kind: tWord, text: word})
			}
		}
	}
	return out, nil
}

func readQuoted(rs []rune, i int) (int, string, error) {
	var b strings.Builder
	for j := i + 1; j < len(rs); j++ {
		if rs[j] == '"' {
			return j + 1, b.String(), nil
		}
		b.WriteRune(rs[j])
	}
	return 0, "", fmt.Errorf("引号未闭合")
}

func readRegex(rs []rune, i int) (int, string, error) {
	var b strings.Builder
	for j := i + 1; j < len(rs); j++ {
		switch rs[j] {
		case '\\':
			if j+1 < len(rs) && rs[j+1] == '/' {
				b.WriteRune('/')
				j++
			} else {
				b.WriteRune('\\')
				if j+1 < len(rs) {
					b.WriteRune(rs[j+1])
					j++
				}
			}
		case '/':
			if b.Len() == 0 {
				return 0, "", fmt.Errorf("正则表达式不能为空")
			}
			return j + 1, b.String(), nil
		default:
			b.WriteRune(rs[j])
		}
	}
	return 0, "", fmt.Errorf("正则表达式缺少结尾的 /")
}

type parser struct {
	toks []tok
	pos  int
}

func (p *parser) peek() *tok {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *parser) describe() string {
	t := p.peek()
	if t == nil {
		return "结尾"
	}
	switch t.kind {
	case tRParen:
		return "右括号"
	case tOr:
		return "OR"
	case tNear:
		return "NEAR"
	}
	return t.text
}

func (p *parser) parseOr() (Node, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	items := []Node{first}
	for t := p.peek(); t != nil && t.kind == tOr; t = p.peek() {
		p.pos++
		n, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	if len(items) == 1 {
		return first, nil
	}
	return &Or{Items: items}, nil
}

func (p *parser) parseAnd() (Node, error) {
	var items []Node
	for {
		t := p.peek()
		if t == nil || t.kind == tOr || t.kind == tRParen {
			break
		}
		n, err := p.parseNear()
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	switch len(items) {
	case 0:
		return nil, fmt.Errorf("缺少搜索条件（位于 %s 附近）", p.describe())
	case 1:
		return items[0], nil
	}
	return &And{Items: items}, nil
}

func (p *parser) parseNear() (Node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t == nil || t.kind != tNear {
		return left, nil
	}
	p.pos++
	right, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	a, ok1 := left.(*Term)
	b, ok2 := right.(*Term)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("NEAR 两侧必须是词或短语")
	}
	return &Near{A: a, B: b, N: t.n}, nil
}

func (p *parser) parseUnary() (Node, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("缺少搜索条件")
	}
	if t.kind == tMinus || t.kind == tNot {
		p.pos++
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &Not{Item: n}, nil
	}
	return p.parseAtom()
}

func (p *parser) parseAtom() (Node, error) {
	t := p.peek()
	p.pos++
	switch t.kind {
	case tLParen:
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c == nil || c.kind != tRParen {
			return nil, fmt.Errorf("括号未闭合")
		}
		p.pos++
		return n, nil
	case tPhrase:
		return &Term{Text: t.text, Phrase: true}, nil
	case tRegex:
		return &Regex{Expr: t.text}, nil
	case tFilter:
		return buildFilter(t)
	case tWord:
		w := t.text
		fuzzy := false
		if strings.HasSuffix(w, "~") && len([]rune(w)) > 1 {
			fuzzy = true
			w = strings.TrimSuffix(w, "~")
		}
		return &Term{Text: w, Fuzzy: fuzzy}, nil
	}
	p.pos--
	return nil, fmt.Errorf("无法理解的 %s", p.describe())
}

func buildFilter(t *tok) (Node, error) {
	if t.text == "" {
		return nil, fmt.Errorf("%s: 后面缺少内容", t.key)
	}
	if t.key == "name" {
		w := t.text
		fuzzy := strings.HasSuffix(w, "~") && len([]rune(w)) > 1
		if fuzzy {
			w = strings.TrimSuffix(w, "~")
		}
		return &Term{Text: w, Fuzzy: fuzzy, NameOnly: true}, nil
	}
	return &Filter{Key: t.key, Value: t.text}, nil
}
