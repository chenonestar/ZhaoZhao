package extract

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// extractXlsNative 直接读取 Excel 97-2003（.xls/.xlt）以及与之兼容的 WPS 表格（.et）中的文字，
// 依据微软公开的 MS-XLS（BIFF8）规范：Workbook 流由一条条记录组成，全局部分有共享字符串表（SST）、
// 单元格格式（XF、FORMAT）和工作表列表，每个工作表的单元格记录引用它们。
// 数字按单元格格式还原日期；公式取保存的计算结果；批注、文本框的文字也提取。
func extractXlsNative(p string) (doc.Result, error) {
	st, all, err := oleStreams(p, "Workbook", "Book")
	if err != nil {
		return doc.Result{}, err
	}
	wb := st["Workbook"]
	if wb == nil {
		if all["Book"] {
			return doc.Result{}, nativeUnsupported("Excel 95 等早期格式")
		}
		return doc.Result{}, nativeUnsupported("没有 Workbook 流")
	}
	return xlsText(wb)
}

// xlsText 解析 Workbook 流（加密的先尝试默认密码解密）。
func xlsText(wb []byte) (doc.Result, error) {
	x := &xlsReader{data: wb}
	err := x.parse()
	var enc xlsEncrypted
	if errors.As(err, &enc) {
		// 加密的：先用 Excel「保护工作簿」的默认密码解密，解不开才是真正设了打开密码
		dec, derr := xlsDecrypt(wb, enc.filepass, xlsDefaultPassword)
		switch {
		case derr == errXlsWrongPassword:
			return doc.Result{}, errNativeNeedPassword
		case derr != nil:
			return doc.Result{}, nativeUnsupported("Excel 的旧式加密（%v）", derr)
		}
		x = &xlsReader{data: dec, decrypted: true}
		err = x.parse()
	}
	if err != nil {
		return doc.Result{}, err
	}
	var b doc.Builder
	for _, sh := range x.sheets {
		if len(sh.cells) == 0 && len(sh.notes) == 0 {
			continue
		}
		rows := map[int][]xlsCell{}
		var rowIDs []int
		for _, c := range sh.cells {
			if _, ok := rows[c.row]; !ok {
				rowIDs = append(rowIDs, c.row)
			}
			rows[c.row] = append(rows[c.row], c)
		}
		sort.Ints(rowIDs)
		for _, r := range rowIDs {
			cs := rows[r]
			sort.Slice(cs, func(i, j int) bool { return cs[i].col < cs[j].col })
			var parts []string
			for _, c := range cs {
				if c.text != "" {
					parts = append(parts, c.text)
				}
			}
			if len(parts) == 0 {
				continue
			}
			b.Mark(sh.name + "!" + colName(cs[0].col) + strconv.Itoa(r+1))
			b.Line(strings.Join(parts, "\t"))
		}
		for _, n := range sh.notes {
			b.Mark(sh.name + "（批注或文本框）")
			b.Line(n)
		}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}, nil
}

type xlsCell struct {
	row, col int
	text     string
}

type xlsSheet struct {
	name  string
	cells []xlsCell
	notes []string
}

// xlsEncrypted 表示 Workbook 流是加密的（遇到 FILEPASS 记录）。
type xlsEncrypted struct{ filepass []byte }

func (xlsEncrypted) Error() string { return "加密的工作簿" }

type xlsReader struct {
	decrypted bool // 已经解密：FILEPASS 记录不再处理
	data      []byte
	sst       []string
	xfFmt     []int          // XF 序号 → 数字格式编号
	formats   map[int]string // 自定义数字格式
	date1904  bool
	sheets    []*xlsSheet
	names     map[int]string // 工作表 BOF 在流中的位置 → 名称
}

type xlsRecord struct {
	typ  int
	data []byte
	pos  int
}

func (x *xlsReader) records() []xlsRecord {
	var out []xlsRecord
	for i := 0; i+4 <= len(x.data); {
		typ, n := le16(x.data, i), le16(x.data, i+2)
		if i+4+n > len(x.data) {
			break
		}
		out = append(out, xlsRecord{typ, x.data[i+4 : i+4+n], i})
		i += 4 + n
	}
	return out
}

const (
	xlsBOF        = 0x0809
	xlsEOF        = 0x000A
	xlsFilePass   = 0x002F
	xlsDateMode   = 0x0022
	xlsFormat     = 0x041E
	xlsXF         = 0x00E0
	xlsBoundSheet = 0x0085
	xlsSST        = 0x00FC
	xlsContinue   = 0x003C
	xlsLabelSST   = 0x00FD
	xlsLabel      = 0x0204
	xlsRString    = 0x00D6
	xlsNumber     = 0x0203
	xlsRK         = 0x027E
	xlsMulRK      = 0x00BD
	xlsFormula    = 0x0006
	xlsString     = 0x0207
	xlsBoolErr    = 0x0205
	xlsTXO        = 0x01B6
)

func (x *xlsReader) parse() error {
	recs := x.records()
	if len(recs) == 0 || recs[0].typ != xlsBOF || le16(recs[0].data, 0) != 0x0600 {
		return nativeUnsupported("不是 BIFF8 格式")
	}
	x.formats = map[int]string{}
	x.names = map[int]string{}
	var cur *xlsSheet
	substream := 0
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		d := r.data
		switch r.typ {
		case xlsBOF:
			substream = le16(d, 2)
			if substream == 0x0010 { // 工作表
				cur = &xlsSheet{name: x.names[r.pos]}
				if cur.name == "" {
					cur.name = "工作表" + strconv.Itoa(len(x.sheets)+1)
				}
				x.sheets = append(x.sheets, cur)
			} else if substream != 0x0005 {
				cur = nil // 图表、宏表：跳过
			}
		case xlsEOF:
			if substream == 0x0010 {
				cur = nil
			}
		case xlsFilePass:
			if !x.decrypted {
				return xlsEncrypted{filepass: d}
			}
		case xlsDateMode:
			x.date1904 = le16(d, 0) == 1
		case xlsFormat:
			if s, _, ok := xlsUnicodeString(d, 2, 2); ok {
				x.formats[le16(d, 0)] = s
			}
		case xlsXF:
			x.xfFmt = append(x.xfFmt, le16(d, 2))
		case xlsBoundSheet:
			if s, _, ok := xlsUnicodeString(d, 6, 1); ok {
				x.names[le32(d, 0)] = s
			}
		case xlsSST:
			segs := [][]byte{d}
			for i+1 < len(recs) && recs[i+1].typ == xlsContinue {
				i++
				segs = append(segs, recs[i].data)
			}
			x.sst = parseSST(segs)
		}
		if cur == nil {
			continue
		}
		switch r.typ {
		case xlsLabelSST:
			if k := le32(d, 6); k < len(x.sst) {
				cur.add(le16(d, 0), le16(d, 2), x.sst[k])
			}
		case xlsLabel, xlsRString:
			if s, _, ok := xlsUnicodeString(d, 6, 2); ok {
				cur.add(le16(d, 0), le16(d, 2), s)
			}
		case xlsNumber:
			if len(d) >= 14 {
				cur.add(le16(d, 0), le16(d, 2), x.number(le16(d, 4), math.Float64frombits(leU64(d, 6))))
			}
		case xlsRK:
			if len(d) >= 10 {
				cur.add(le16(d, 0), le16(d, 2), x.number(le16(d, 4), rkValue(uint32(le32(d, 6)))))
			}
		case xlsMulRK:
			row, first := le16(d, 0), le16(d, 2)
			for k, off := 0, 4; off+6 <= len(d)-2; k, off = k+1, off+6 {
				cur.add(row, first+k, x.number(le16(d, off), rkValue(uint32(le32(d, off+2)))))
			}
		case xlsFormula:
			if len(d) < 14 {
				continue
			}
			row, col, xf := le16(d, 0), le16(d, 2), le16(d, 4)
			if d[12] == 0xFF && d[13] == 0xFF {
				switch d[6] {
				case 0: // 文字结果在随后的 STRING 记录里
					for j := i + 1; j < len(recs) && j <= i+3; j++ {
						if recs[j].typ == xlsString {
							segs := [][]byte{recs[j].data}
							for j+1 < len(recs) && recs[j+1].typ == xlsContinue {
								j++
								segs = append(segs, recs[j].data)
							}
							if s, ok := continuedString(segs); ok {
								cur.add(row, col, s)
							}
							break
						}
					}
				case 1:
					cur.add(row, col, map[bool]string{true: "TRUE", false: "FALSE"}[d[8] != 0])
				}
			} else {
				cur.add(row, col, x.number(xf, math.Float64frombits(leU64(d, 6))))
			}
		case xlsBoolErr:
			if len(d) >= 8 && d[7] == 0 {
				cur.add(le16(d, 0), le16(d, 2), map[bool]string{true: "TRUE", false: "FALSE"}[d[6] != 0])
			}
		case xlsTXO: // 批注、文本框：文字在随后的 CONTINUE 记录里
			cch := le16(d, 10)
			if cch > 0 && i+1 < len(recs) && recs[i+1].typ == xlsContinue {
				i++
				var segs [][]byte
				segs = append(segs, recs[i].data)
				for i+1 < len(recs) && recs[i+1].typ == xlsContinue && len(segs) < 64 {
					// 后面的 CONTINUE 是格式运行，不是文字
					break
				}
				if s := txoText(segs[0], cch); strings.TrimSpace(s) != "" {
					cur.notes = append(cur.notes, s)
				}
			}
		}
	}
	return nil
}

func (s *xlsSheet) add(row, col int, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.cells = append(s.cells, xlsCell{row, col, text})
}

func leU64(b []byte, off int) uint64 {
	return uint64(uint32(le32(b, off))) | uint64(uint32(le32(b, off+4)))<<32
}

// rkValue 解码 RK 压缩数字。
func rkValue(rk uint32) float64 {
	var v float64
	if rk&0x02 != 0 {
		v = float64(int32(rk) >> 2)
	} else {
		v = math.Float64frombits(uint64(rk&0xFFFFFFFC) << 32)
	}
	if rk&0x01 != 0 {
		v /= 100
	}
	return v
}

// xlsUnicodeString 读取 XLUnicodeString（lenSize=2）或 ShortXLUnicodeString（lenSize=1）。
func xlsUnicodeString(d []byte, off, lenSize int) (string, int, bool) {
	if off+lenSize+1 > len(d) {
		return "", 0, false
	}
	cch := int(d[off])
	if lenSize == 2 {
		cch = le16(d, off)
	}
	flags := d[off+lenSize]
	p := off + lenSize + 1
	if flags&0x01 != 0 {
		if p+2*cch > len(d) {
			return "", 0, false
		}
		u := make([]uint16, cch)
		for i := range u {
			u[i] = uint16(le16(d, p+2*i))
		}
		return string(utf16.Decode(u)), p + 2*cch, true
	}
	if p+cch > len(d) {
		return "", 0, false
	}
	return latin1(d[p : p+cch]), p + cch, true
}

func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// segReader 顺序读取跨多个记录（SST 及其 CONTINUE）的数据。字符串的字符部分跨记录时，
// 新记录的第一个字节是新的「是否双字节」标志。
type segReader struct {
	segs [][]byte
	si   int
	off  int
}

func (r *segReader) ok() bool { return r.si < len(r.segs) }

func (r *segReader) advance() {
	for r.si < len(r.segs) && r.off >= len(r.segs[r.si]) {
		r.si++
		r.off = 0
	}
}

func (r *segReader) byte1() (byte, bool) {
	r.advance()
	if !r.ok() {
		return 0, false
	}
	b := r.segs[r.si][r.off]
	r.off++
	return b, true
}

func (r *segReader) u16() (int, bool) {
	a, ok1 := r.byte1()
	b, ok2 := r.byte1()
	return int(a) | int(b)<<8, ok1 && ok2
}

func (r *segReader) u32() (int, bool) {
	a, ok1 := r.u16()
	b, ok2 := r.u16()
	return a | b<<16, ok1 && ok2
}

func (r *segReader) skip(n int) {
	for n > 0 && r.ok() {
		r.advance()
		if !r.ok() {
			return
		}
		k := len(r.segs[r.si]) - r.off
		if k > n {
			k = n
		}
		r.off += k
		n -= k
	}
}

// chars 读取 cch 个字符；跨记录时读新的标志字节。
func (r *segReader) chars(cch int, high bool) (string, bool) {
	u := make([]uint16, 0, cch)
	for len(u) < cch {
		r.advance()
		if !r.ok() {
			return "", false
		}
		if r.off == 0 && len(u) > 0 { // 新的 CONTINUE 记录：先读标志
			f, _ := r.byte1()
			high = f&0x01 != 0
		}
		seg := r.segs[r.si]
		for len(u) < cch && r.off < len(seg) {
			if high {
				if r.off+2 > len(seg) {
					r.off = len(seg)
					break
				}
				u = append(u, uint16(seg[r.off])|uint16(seg[r.off+1])<<8)
				r.off += 2
			} else {
				u = append(u, uint16(seg[r.off]))
				r.off++
			}
		}
	}
	return string(utf16.Decode(u)), true
}

// parseSST 解析共享字符串表（XLUnicodeRichExtendedString 序列）。
func parseSST(segs [][]byte) []string {
	r := &segReader{segs: segs}
	r.skip(4) // cstTotal
	n, ok := r.u32()
	if !ok || n < 0 || n > 1<<24 {
		return nil
	}
	out := make([]string, 0, min(n, 1<<16)) // 数量来自文件头，不可信：不按它预先分配
	for i := 0; i < n; i++ {
		cch, ok := r.u16()
		if !ok {
			break
		}
		flags, ok := r.byte1()
		if !ok {
			break
		}
		runs, ext := 0, 0
		if flags&0x08 != 0 {
			runs, _ = r.u16()
		}
		if flags&0x04 != 0 {
			ext, _ = r.u32()
		}
		s, ok := r.chars(cch, flags&0x01 != 0)
		if !ok {
			break
		}
		r.skip(4*runs + ext)
		out = append(out, s)
	}
	return out
}

// continuedString 读取 STRING 记录（可能跨 CONTINUE）里的 XLUnicodeString。
func continuedString(segs [][]byte) (string, bool) {
	r := &segReader{segs: segs}
	cch, ok := r.u16()
	if !ok {
		return "", false
	}
	f, ok := r.byte1()
	if !ok {
		return "", false
	}
	return r.chars(cch, f&0x01 != 0)
}

// txoText 读取批注、文本框的文字（TXO 之后第一个 CONTINUE 记录：标志字节 + 字符）。
func txoText(d []byte, cch int) string {
	if len(d) < 1 {
		return ""
	}
	if d[0]&0x01 != 0 {
		n := (len(d) - 1) / 2
		if n > cch {
			n = cch
		}
		u := make([]uint16, n)
		for i := range u {
			u[i] = uint16(le16(d, 1+2*i))
		}
		return string(utf16.Decode(u))
	}
	b := d[1:]
	if len(b) > cch {
		b = b[:cch]
	}
	return latin1(b)
}

// number 按单元格格式把数字转成文字：日期格式还原成日期，其余输出不带多余零的数字。
func (x *xlsReader) number(xf int, v float64) string {
	if xf >= 0 && xf < len(x.xfFmt) && x.isDateFormat(x.xfFmt[xf]) && v >= 0 && v < 2958466 {
		return excelDate(v, x.date1904)
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (x *xlsReader) isDateFormat(ifmt int) bool {
	switch {
	case ifmt >= 14 && ifmt <= 22, ifmt >= 27 && ifmt <= 36, ifmt >= 45 && ifmt <= 47, ifmt >= 50 && ifmt <= 58:
		return true
	}
	f, ok := x.formats[ifmt]
	if !ok {
		return false
	}
	return isDateFormatString(f)
}

// isDateFormatString 判断自定义数字格式是否是日期/时间：去掉引号里的文字和方括号部分后，含有 y、m、d、h、s。
func isDateFormatString(f string) bool {
	var sb strings.Builder
	inQ, inB := false, false
	for _, r := range f {
		switch {
		case r == '"':
			inQ = !inQ
		case inQ:
		case r == '[':
			inB = true
		case r == ']':
			inB = false
		case inB:
		default:
			sb.WriteRune(r)
		}
	}
	s := strings.ToLower(sb.String())
	if strings.Contains(s, "general") {
		return false
	}
	return strings.ContainsAny(s, "ymdhs") && !strings.ContainsAny(s, "#0?")
}

// excelDate 把 Excel 的日期序号转成「2006-01-02」（有时间部分时加「15:04:05」）。
func excelDate(v float64, date1904 bool) string {
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	} else if v < 61 {
		base = time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC) // Excel 把 1900 年当作闰年
	}
	days := math.Floor(v)
	secs := math.Round((v - days) * 86400)
	t := base.AddDate(0, 0, int(days)).Add(time.Duration(secs) * time.Second)
	if secs == 0 {
		return t.Format("2006-01-02")
	}
	if days == 0 {
		return t.Format("15:04:05")
	}
	return t.Format("2006-01-02 15:04:05")
}

func colName(c int) string {
	s := ""
	for c++; c > 0; c = (c - 1) / 26 {
		s = string(rune('A'+(c-1)%26)) + s
	}
	return s
}
