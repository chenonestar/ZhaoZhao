package search

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/query"
	"github.com/chenonestar/zhaozhao/internal/store"
)

var typeNames = map[string]string{
	"文档": extract.KindDoc, "doc": extract.KindDoc, "document": extract.KindDoc,
	"表格": extract.KindSheet, "sheet": extract.KindSheet,
	"演示": extract.KindSlide, "slide": extract.KindSlide,
	"pdf": extract.KindPDF, "PDF": extract.KindPDF,
	"图片": extract.KindImage, "image": extract.KindImage,
	"文本": extract.KindText, "text": extract.KindText,
	"代码": extract.KindCode, "code": extract.KindCode,
	"压缩包": extract.KindArchive, "archive": extract.KindArchive,
	"邮件": extract.KindMail, "mail": extract.KindMail,
}

func checkFilter(f *query.Filter) error {
	switch f.Key {
	case "type":
		for _, v := range strings.Split(f.Value, ",") {
			if _, ok := typeNames[strings.ToLower(strings.TrimSpace(v))]; !ok {
				if _, ok := typeNames[strings.TrimSpace(v)]; !ok {
					return fmt.Errorf("type: 不认识「%s」，可用：文档、表格、演示、PDF、图片、文本、代码、压缩包、邮件", v)
				}
			}
		}
	case "size":
		if _, err := parseSize(f.Value); err != nil {
			return err
		}
	case "modified":
		if _, err := parseTimeRange(f.Value, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

// ---- size ----

type i64Range struct {
	lo, hi       int64
	hasLo, hasHi bool
}

func (r i64Range) has(v int64) bool {
	return (!r.hasLo || v >= r.lo) && (!r.hasHi || v <= r.hi)
}

var sizeRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([a-zA-Z]*)$`)

func sizeOne(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("size: 无法理解「%s」，示例：size:>10MB", s)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	mult := int64(1)
	switch strings.ToUpper(m[2]) {
	case "", "B":
	case "K", "KB":
		mult = 1 << 10
	case "M", "MB":
		mult = 1 << 20
	case "G", "GB":
		mult = 1 << 30
	case "T", "TB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("size: 未知单位「%s」", m[2])
	}
	return int64(n * float64(mult)), nil
}

func parseSize(v string) (i64Range, error) {
	var r i64Range
	if i := strings.Index(v, ".."); i >= 0 {
		if a := v[:i]; a != "" {
			n, err := sizeOne(a)
			if err != nil {
				return r, err
			}
			r.lo, r.hasLo = n, true
		}
		if b := v[i+2:]; b != "" {
			n, err := sizeOne(b)
			if err != nil {
				return r, err
			}
			r.hi, r.hasHi = n, true
		}
		return r, nil
	}
	op := ""
	for _, p := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(v, p) {
			op, v = p, v[len(p):]
			break
		}
	}
	n, err := sizeOne(v)
	if err != nil {
		return r, err
	}
	switch op {
	case ">":
		r.lo, r.hasLo = n+1, true
	case ">=":
		r.lo, r.hasLo = n, true
	case "<":
		r.hi, r.hasHi = n-1, true
	case "<=":
		r.hi, r.hasHi = n, true
	default:
		r.lo, r.hasLo, r.hi, r.hasHi = n, true, n, true
	}
	return r, nil
}

// ---- modified ----

var (
	dateRe = regexp.MustCompile(`^(\d{4})(?:[-/.](\d{1,2}))?(?:[-/.](\d{1,2}))?$`)
	relRe  = regexp.MustCompile(`^(\d+)([hdwmyHDWMY])$`)
)

// parseDate 返回日期（可只写年或年月）对应的 [起, 止) 区间。
func parseDate(s string) (time.Time, time.Time, error) {
	m := dateRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("modified: 无法理解日期「%s」，示例：2025-03-01", s)
	}
	y, _ := strconv.Atoi(m[1])
	loc := time.Local
	switch {
	case m[3] != "":
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		st := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
		return st, st.AddDate(0, 0, 1), nil
	case m[2] != "":
		mo, _ := strconv.Atoi(m[2])
		st := time.Date(y, time.Month(mo), 1, 0, 0, 0, 0, loc)
		return st, st.AddDate(0, 1, 0), nil
	}
	st := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	return st, st.AddDate(1, 0, 0), nil
}

func relDuration(s string) (time.Duration, bool) {
	m := relRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "m": 30 * 24 * time.Hour, "y": 365 * 24 * time.Hour}
	return time.Duration(n) * unit[strings.ToLower(m[2])], true
}

// parseTimeRange 解析 modified: 的值，返回允许的 Unix 秒区间。
// 相对时间：<30d 表示最近 30 天内，>30d 表示 30 天之前。
func parseTimeRange(v string, now time.Time) (i64Range, error) {
	var r i64Range
	if i := strings.Index(v, ".."); i >= 0 {
		if a := v[:i]; a != "" {
			st, _, err := parseDate(a)
			if err != nil {
				return r, err
			}
			r.lo, r.hasLo = st.Unix(), true
		}
		if b := v[i+2:]; b != "" {
			_, en, err := parseDate(b)
			if err != nil {
				return r, err
			}
			r.hi, r.hasHi = en.Unix()-1, true
		}
		return r, nil
	}
	op := ""
	for _, p := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(v, p) {
			op, v = p, v[len(p):]
			break
		}
	}
	if d, ok := relDuration(v); ok {
		cut := now.Add(-d).Unix()
		switch op {
		case ">", ">=":
			r.hi, r.hasHi = cut, true
		default:
			r.lo, r.hasLo = cut, true
		}
		return r, nil
	}
	st, en, err := parseDate(v)
	if err != nil {
		return r, err
	}
	switch op {
	case ">":
		r.lo, r.hasLo = en.Unix(), true
	case ">=":
		r.lo, r.hasLo = st.Unix(), true
	case "<":
		r.hi, r.hasHi = st.Unix()-1, true
	case "<=":
		r.hi, r.hasHi = en.Unix()-1, true
	default:
		r.lo, r.hasLo, r.hi, r.hasHi = st.Unix(), true, en.Unix()-1, true
	}
	return r, nil
}

// ---- 逐个文件的元数据判断 ----

type mailMeta struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
}

type metaEval struct {
	now       time.Time
	norm      norm.Options
	path      string // 显示路径
	volLabel  string
	file      *store.File
	mail      *mailMeta
	typeCache map[string]map[string]bool
}

func (m *metaEval) contains(hay, needle string) bool {
	return strings.Contains(m.norm.String(hay), m.norm.String(needle))
}

func (m *metaEval) filter(f *query.Filter) bool {
	switch f.Key {
	case "path":
		p := strings.ReplaceAll(m.path, "/", `\`)
		return m.contains(p, strings.ReplaceAll(f.Value, "/", `\`))
	case "ext":
		for _, e := range strings.Split(f.Value, ",") {
			if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(e), "."), m.file.Ext) {
				return true
			}
		}
		return false
	case "type":
		for _, v := range strings.Split(f.Value, ",") {
			k, ok := typeNames[strings.ToLower(strings.TrimSpace(v))]
			if !ok {
				k = typeNames[strings.TrimSpace(v)]
			}
			if m.file.Kind == k {
				return true
			}
		}
		return false
	case "size":
		r, _ := parseSize(f.Value)
		return r.has(m.file.Size)
	case "modified":
		r, _ := parseTimeRange(f.Value, m.now)
		return r.has(m.file.MTime)
	case "vol":
		return m.contains(m.volLabel, f.Value)
	case "from", "to", "subject":
		if m.mail == nil {
			if m.file.Meta == "" {
				return false
			}
			m.mail = &mailMeta{}
			_ = json.Unmarshal([]byte(m.file.Meta), m.mail)
		}
		switch f.Key {
		case "from":
			return m.contains(m.mail.From, f.Value)
		case "to":
			return m.contains(m.mail.To, f.Value)
		}
		return m.contains(m.mail.Subject, f.Value)
	}
	return false
}

// 三值逻辑：词、正则等需要读取文本的条件在元数据阶段为「未知」。
const (
	triUnknown = iota
	triTrue
	triFalse
)

func (m *metaEval) tri(n query.Node) int {
	switch t := n.(type) {
	case *query.Filter:
		if m.filter(t) {
			return triTrue
		}
		return triFalse
	case *query.And:
		res := triTrue
		for _, c := range t.Items {
			switch m.tri(c) {
			case triFalse:
				return triFalse
			case triUnknown:
				res = triUnknown
			}
		}
		return res
	case *query.Or:
		res := triFalse
		for _, c := range t.Items {
			switch m.tri(c) {
			case triTrue:
				return triTrue
			case triUnknown:
				res = triUnknown
			}
		}
		return res
	case *query.Not:
		switch m.tri(t.Item) {
		case triTrue:
			return triFalse
		case triFalse:
			return triTrue
		}
		return triUnknown
	}
	return triUnknown
}
