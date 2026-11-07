// Package query 解析「找找」的搜索语法（见需求说明第 5 节）。
package query

// Node 是查询语法树节点。
type Node interface{ node() }

type (
	// And 要求所有子节点同时满足。
	And struct{ Items []Node }
	// Or 要求任一子节点满足。
	Or struct{ Items []Node }
	// Not 要求子节点不满足。
	Not struct{ Item Node }
	// Term 是一个词或短语。Phrase 表示用引号括起；Fuzzy 表示带 ~ 后缀。
	// NameOnly 表示只匹配文件名（name: 前缀）。
	Term struct {
		Text     string
		Phrase   bool
		Fuzzy    bool
		NameOnly bool
	}
	// Near 要求两个词相距不超过 N 个字，不限先后。
	Near struct {
		A, B *Term
		N    int
	}
	// Regex 是 /.../ 正则（RE2 语法）。
	Regex struct{ Expr string }
	// Filter 是 key:value 形式的元数据条件。
	Filter struct{ Key, Value string }
)

func (*And) node()    {}
func (*Or) node()     {}
func (*Not) node()    {}
func (*Term) node()   {}
func (*Near) node()   {}
func (*Regex) node()  {}
func (*Filter) node() {}
