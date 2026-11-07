package extract

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

const maxXMLPart = 256 << 20

var errEncrypted = fmt.Errorf("encrypted")

// openZip 打开 zip 容器；老式加密的 Office 文件是 OLE 复合文档，会被识别为已加密。
func openZip(p string) (*zip.ReadCloser, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		if isOLE(p) {
			return nil, errEncrypted
		}
		return nil, err
	}
	return zr, nil
}

func isOLE(p string) bool {
	b, err := readHead(p, 8)
	return err == nil && len(b) == 8 && string(b) == "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"
}

func findPart(zr *zip.ReadCloser, name string) *zip.File {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

func partsMatching(zr *zip.ReadCloser, re *regexp.Regexp) []*zip.File {
	var out []*zip.File
	for _, f := range zr.File {
		if re.MatchString(f.Name) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].Name, out[j].Name) })
	return out
}

var digitRun = regexp.MustCompile(`\d+`)

func naturalLess(a, b string) bool {
	na, nb := digitRun.FindString(a), digitRun.FindString(b)
	if na != "" && nb != "" {
		ia, _ := strconv.Atoi(na)
		ib, _ := strconv.Atoi(nb)
		if ia != ib {
			return ia < ib
		}
	}
	return a < b
}

func newDecoder(f *zip.File) (*xml.Decoder, io.Closer, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, nil, err
	}
	d := xml.NewDecoder(io.LimitReader(rc, maxXMLPart))
	d.Strict = false
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) { return in, nil }
	return d, rc, nil
}

// ---- docx ----

var docxPartRe = regexp.MustCompile(`(?i)^word/(document|header\d*|footer\d*|footnotes|endnotes|comments)\.xml$`)

func extractDocx(p string) doc.Result {
	zr, err := openZip(p)
	if err != nil {
		return openErr(err)
	}
	defer zr.Close()
	var b doc.Builder
	parts := partsMatching(zr, docxPartRe)
	// 正文放最前面
	sort.SliceStable(parts, func(i, j int) bool {
		return strings.EqualFold(parts[i].Name, "word/document.xml") && !strings.EqualFold(parts[j].Name, "word/document.xml")
	})
	if len(parts) == 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "不是有效的 Word 文档"}
	}
	for _, f := range parts {
		label := docxLabel(f.Name)
		if err := walkParagraphs(f, &b, label, "t", "p", map[string]string{"tab": "\t", "br": "\n", "cr": "\n"}, "delText", "instrText"); err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
		}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}
}

func docxLabel(name string) string {
	n := strings.ToLower(path.Base(name))
	switch {
	case n == "document.xml":
		return ""
	case strings.HasPrefix(n, "header"):
		return "页眉"
	case strings.HasPrefix(n, "footer"):
		return "页脚"
	case strings.HasPrefix(n, "footnotes"):
		return "脚注"
	case strings.HasPrefix(n, "endnotes"):
		return "尾注"
	case strings.HasPrefix(n, "comments"):
		return "批注"
	}
	return n
}

// walkParagraphs 遍历 XML，收集文本元素 textTag 的内容，在每个 paraTag 结束处换行。
// label 为空时用「第 N 段」编号；否则用固定标签（页眉、脚注等）。
func walkParagraphs(f *zip.File, b *doc.Builder, label, textTag, paraTag string, special map[string]string, skipTags ...string) error {
	d, c, err := newDecoder(f)
	if err != nil {
		return err
	}
	defer c.Close()
	skip := map[string]bool{}
	for _, s := range skipTags {
		skip[s] = true
	}
	para := 0
	inText := false
	skipDepth := 0
	var line strings.Builder
	started := false
	flush := func() {
		s := strings.TrimRight(line.String(), " \t")
		line.Reset()
		if strings.TrimSpace(s) == "" {
			started = false
			return
		}
		para++
		l := label
		if l == "" {
			l = "第" + strconv.Itoa(para) + "段"
		}
		b.Mark(l)
		b.Line(s)
		started = false
	}
	_ = started
	for {
		tk, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if b.Len() > 0 {
				break // 尽力而为：保留已解析部分
			}
			return err
		}
		switch t := tk.(type) {
		case xml.StartElement:
			n := t.Name.Local
			switch {
			case skip[n]:
				skipDepth++
			case skipDepth > 0:
			case n == textTag:
				inText = true
			default:
				if s, ok := special[n]; ok {
					line.WriteString(s)
				}
			}
		case xml.EndElement:
			n := t.Name.Local
			switch {
			case skip[n]:
				if skipDepth > 0 {
					skipDepth--
				}
			case skipDepth > 0:
			case n == textTag:
				inText = false
			case n == paraTag:
				flush()
			}
		case xml.CharData:
			if inText && skipDepth == 0 {
				line.Write(t)
			}
		}
	}
	flush()
	return nil
}

// ---- pptx ----

var slideRe = regexp.MustCompile(`(?i)^ppt/slides/slide\d+\.xml$`)

func extractPptx(p string) doc.Result {
	zr, err := openZip(p)
	if err != nil {
		return openErr(err)
	}
	defer zr.Close()
	slides := partsMatching(zr, slideRe)
	if len(slides) == 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "不是有效的演示文稿"}
	}
	var b doc.Builder
	for i, s := range slides {
		label := "幻灯片 " + strconv.Itoa(i+1)
		if err := walkParagraphs(s, &b, label, "t", "p", map[string]string{"br": "\n", "tab": "\t"}); err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
		}
		// 备注
		if notes := relTarget(zr, s.Name, "notesSlide"); notes != nil {
			if err := walkParagraphs(notes, &b, label+" 备注", "t", "p", nil); err != nil {
				return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
			}
		}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}
}

type relationships struct {
	Rels []struct {
		ID     string `xml:"Id,attr"`
		Type   string `xml:"Type,attr"`
		Target string `xml:"Target,attr"`
		Mode   string `xml:"TargetMode,attr"`
	} `xml:"Relationship"`
}

func readRels(zr *zip.ReadCloser, part string) *relationships {
	rf := findPart(zr, path.Join(path.Dir(part), "_rels", path.Base(part)+".rels"))
	if rf == nil {
		return nil
	}
	rc, err := rf.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	var r relationships
	if xml.NewDecoder(io.LimitReader(rc, 8<<20)).Decode(&r) != nil {
		return nil
	}
	return &r
}

func resolveTarget(part, target string) string {
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(target, "/")
	}
	return path.Clean(path.Join(path.Dir(part), target))
}

// relTarget 在 part 的关系文件里按类型后缀找目标部件。
func relTarget(zr *zip.ReadCloser, part, typeSuffix string) *zip.File {
	r := readRels(zr, part)
	if r == nil {
		return nil
	}
	for _, rel := range r.Rels {
		if strings.HasSuffix(rel.Type, "/"+typeSuffix) && rel.Mode != "External" {
			return findPart(zr, resolveTarget(part, rel.Target))
		}
	}
	return nil
}

// ---- xlsx ----

func extractXlsx(p string) doc.Result {
	zr, err := openZip(p)
	if err != nil {
		return openErr(err)
	}
	defer zr.Close()
	wb := findPart(zr, "xl/workbook.xml")
	if wb == nil {
		return doc.Result{Status: doc.StatusFailed, Err: "不是有效的 Excel 文档"}
	}
	shared := readSharedStrings(zr)

	type sheetRef struct{ name, rid string }
	var sheets []sheetRef
	d, c, err := newDecoder(wb)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	for {
		tk, err := d.Token()
		if err != nil {
			break
		}
		if se, ok := tk.(xml.StartElement); ok && se.Name.Local == "sheet" {
			var s sheetRef
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "name":
					s.name = a.Value
				case "id":
					s.rid = a.Value
				}
			}
			sheets = append(sheets, s)
		}
	}
	c.Close()
	rels := readRels(zr, "xl/workbook.xml")
	targets := map[string]string{}
	if rels != nil {
		for _, r := range rels.Rels {
			targets[r.ID] = resolveTarget("xl/workbook.xml", r.Target)
		}
	}
	var b doc.Builder
	for _, s := range sheets {
		f := findPart(zr, targets[s.rid])
		if f == nil {
			continue
		}
		if err := readSheet(f, s.name, shared, &b); err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
		}
		if cm := relTarget(zr, f.Name, "comments"); cm != nil {
			readComments(cm, s.name, &b)
		}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}
}

func readSharedStrings(zr *zip.ReadCloser) []string {
	f := findPart(zr, "xl/sharedStrings.xml")
	if f == nil {
		return nil
	}
	d, c, err := newDecoder(f)
	if err != nil {
		return nil
	}
	defer c.Close()
	var out []string
	var cur strings.Builder
	inSI, inT, inPh := false, false, 0
	for {
		tk, err := d.Token()
		if err != nil {
			break
		}
		switch t := tk.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = true
			case "rPh":
				inPh++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				out = append(out, cur.String())
				inSI = false
			case "t":
				inT = false
			case "rPh":
				if inPh > 0 {
					inPh--
				}
			}
		case xml.CharData:
			if inSI && inT && inPh == 0 {
				cur.Write(t)
			}
		}
	}
	return out
}

func readSheet(f *zip.File, name string, shared []string, b *doc.Builder) error {
	d, c, err := newDecoder(f)
	if err != nil {
		return err
	}
	defer c.Close()
	var (
		ref, typ string
		val      strings.Builder
		inV, inT bool
		inIS     bool
		rowCells int
	)
	endCell := func() {
		s := val.String()
		if typ == "s" {
			if i, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && i >= 0 && i < len(shared) {
				s = shared[i]
			} else {
				s = ""
			}
		} else if typ == "b" {
			if strings.TrimSpace(s) == "1" {
				s = "TRUE"
			} else {
				s = "FALSE"
			}
		} else if typ == "e" {
			s = ""
		}
		s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " "))
		if s == "" {
			return
		}
		if rowCells > 0 {
			b.WriteString("\t")
		}
		b.Mark(name + "!" + ref)
		b.WriteString(s)
		rowCells++
	}
	for {
		tk, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if b.Len() > 0 {
				break
			}
			return err
		}
		switch t := tk.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				rowCells = 0
			case "c":
				ref, typ = "", ""
				val.Reset()
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "r":
						ref = a.Value
					case "t":
						typ = a.Value
					}
				}
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "is":
				inIS = false
			case "t":
				inT = false
			case "c":
				endCell()
			case "row":
				if rowCells > 0 {
					b.WriteString("\n")
				}
			}
		case xml.CharData:
			if inV || (inIS && inT) {
				val.Write(t)
			}
		}
	}
	return nil
}

func readComments(f *zip.File, sheet string, b *doc.Builder) {
	d, c, err := newDecoder(f)
	if err != nil {
		return
	}
	defer c.Close()
	var ref string
	var cur strings.Builder
	inT := false
	for {
		tk, err := d.Token()
		if err != nil {
			return
		}
		switch t := tk.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "comment":
				cur.Reset()
				for _, a := range t.Attr {
					if a.Name.Local == "ref" {
						ref = a.Value
					}
				}
			case "t":
				inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "comment":
				if s := strings.TrimSpace(cur.String()); s != "" {
					b.Mark(sheet + "!" + ref + "（批注）")
					b.Line(s)
				}
			}
		case xml.CharData:
			if inT {
				cur.Write(t)
			}
		}
	}
}

// ---- OpenDocument ----

func extractODF(p string) doc.Result {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return openErr(err)
	}
	defer zr.Close()
	f := findPart(zr, "content.xml")
	if f == nil {
		return doc.Result{Status: doc.StatusFailed, Err: "不是有效的 OpenDocument 文件"}
	}
	d, c, err := newDecoder(f)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	defer c.Close()
	var b doc.Builder
	var line strings.Builder
	n := 0
	flush := func() {
		s := strings.TrimSpace(line.String())
		line.Reset()
		if s != "" {
			n++
			b.Mark("第" + strconv.Itoa(n) + "段")
			b.Line(s)
		}
	}
	for {
		tk, err := d.Token()
		if err != nil {
			break
		}
		switch t := tk.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "s":
				line.WriteByte(' ')
			case "tab":
				line.WriteByte('\t')
			case "line-break":
				line.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p", "h", "table-row":
				flush()
			case "table-cell":
				line.WriteByte('\t')
			}
		case xml.CharData:
			line.Write(t)
		}
	}
	flush()
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}
}

func openErr(err error) doc.Result {
	if err == errEncrypted {
		return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}
	}
	return doc.Result{Status: doc.StatusFailed, Err: "文件已损坏或格式不正确: " + err.Error()}
}
