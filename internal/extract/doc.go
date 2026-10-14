package extract

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"golang.org/x/text/encoding/charmap"
)

// extractDocNative 直接读取 Word 97-2003（.doc/.dot）以及与之兼容的 WPS 文字（.wps）中的文字，
// 不启动 LibreOffice。依据微软公开的 MS-DOC 规范：
//
//	WordDocument 流开头是 FIB；FIB 里的 fcClx/lcbClx 指向表格流（0Table 或 1Table）中的 CLX；
//	CLX 里的 PlcPcd（片段表）给出每段文字在 WordDocument 流里的位置，以及它是 UTF-16 还是单字节（cp1252）。
//
// 文字按 CP 顺序依次是：正文、脚注、页眉页脚、批注、尾注、文本框、页眉文本框。
func extractDocNative(p string) (doc.Result, error) {
	st, _, err := oleStreams(p, "WordDocument", "0Table", "1Table")
	if err != nil {
		return doc.Result{}, err
	}
	wd := st["WordDocument"]
	if len(wd) < 0x200 {
		return doc.Result{}, nativeUnsupported("没有 WordDocument 流")
	}
	if le16(wd, 0) != 0xA5EC {
		return doc.Result{}, nativeUnsupported("不是 Word 97 及以后的格式（wIdent=%#x）", le16(wd, 0))
	}
	if nFib := le16(wd, 2); nFib < 0xC0 {
		return doc.Result{}, nativeUnsupported("Word 6/95 等早期格式（nFib=%#x）", nFib)
	}
	flags := le16(wd, 0x0A)
	if flags&0x0100 != 0 { // fEncrypted（包括 XOR 混淆）
		return doc.Result{}, errNativeEncrypted
	}
	tbl := st["0Table"]
	if flags&0x0200 != 0 { // fWhichTblStm
		tbl = st["1Table"]
	}
	if len(tbl) == 0 {
		return doc.Result{}, nativeUnsupported("没有表格流")
	}
	// FibRgW97、FibRgLw97、FibRgFcLcb 的长度都写在文件里，按实际长度定位
	csw := le16(wd, 32)
	lwOff := 34 + csw*2 + 2
	cslw := le16(wd, 34+csw*2)
	if cslw < 22 {
		return doc.Result{}, nativeUnsupported("FIB 结构异常")
	}
	ccp := []int{
		le32(wd, lwOff+3*4),  // ccpText 正文
		le32(wd, lwOff+4*4),  // ccpFtn 脚注
		le32(wd, lwOff+5*4),  // ccpHdd 页眉页脚
		le32(wd, lwOff+6*4),  // ccpMcr（未用）
		le32(wd, lwOff+7*4),  // ccpAtn 批注
		le32(wd, lwOff+8*4),  // ccpEdn 尾注
		le32(wd, lwOff+9*4),  // ccpTxbx 文本框
		le32(wd, lwOff+10*4), // ccpHdrTxbx 页眉文本框
	}
	rgOff := lwOff + cslw*4
	if le16(wd, rgOff) < 34 {
		return doc.Result{}, nativeUnsupported("FIB 结构异常")
	}
	fcl := rgOff + 2
	fcClx, lcbClx := le32(wd, fcl+33*8), le32(wd, fcl+33*8+4)
	if lcbClx <= 0 || fcClx+lcbClx > len(tbl) {
		return doc.Result{}, nativeUnsupported("片段表位置无效")
	}
	runes, err := docPieces(wd, tbl[fcClx:fcClx+lcbClx])
	if err != nil {
		return doc.Result{}, err
	}

	var b doc.Builder
	labels := []string{"", "脚注", "页眉页脚", "", "批注", "尾注", "文本框", "页眉文本框"}
	pos, para := 0, 0
	for i, n := range ccp {
		if n < 0 || pos+n > len(runes) {
			n = len(runes) - pos
		}
		if n <= 0 {
			continue
		}
		part := docClean(runes[pos : pos+n])
		pos += n
		if labels[i] == "" && i != 0 {
			continue
		}
		for _, line := range strings.Split(part, "\n") {
			if line = strings.TrimRight(line, " \t"); strings.TrimSpace(line) == "" {
				continue
			}
			l := labels[i]
			if i == 0 {
				para++
				l = "第" + strconv.Itoa(para) + "段"
			}
			b.Mark(l)
			b.Line(line)
		}
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}, nil
}

var cp1252 = charmap.Windows1252.NewDecoder()

// docPieces 按片段表把全部文字拼起来（按 CP 顺序）。
func docPieces(wd, clx []byte) ([]rune, error) {
	i := 0
	for i < len(clx) && clx[i] == 0x01 { // Prc：格式信息，跳过
		cb := le16(clx, i+1)
		i += 3 + cb
	}
	if i >= len(clx) || clx[i] != 0x02 {
		return nil, nativeUnsupported("片段表格式异常")
	}
	lcb := le32(clx, i+1)
	plc := clx[i+5:]
	if lcb > len(plc) || lcb < 16 || (lcb-4)%12 != 0 {
		return nil, nativeUnsupported("片段表长度异常")
	}
	plc = plc[:lcb]
	n := (lcb - 4) / 12
	var out []rune
	for k := 0; k < n; k++ {
		cpStart, cpEnd := le32(plc, k*4), le32(plc, (k+1)*4)
		cnt := cpEnd - cpStart
		if cnt <= 0 || cnt > 1<<26 {
			continue
		}
		pcd := (n+1)*4 + k*8
		fcRaw := le32(plc, pcd+2)
		fc := fcRaw & 0x3FFFFFFF
		if fcRaw&0x40000000 != 0 { // 单字节（cp1252）
			off := fc / 2
			if off+cnt > len(wd) {
				return nil, nativeUnsupported("片段超出文件范围")
			}
			s, _ := cp1252.Bytes(wd[off : off+cnt])
			out = append(out, []rune(string(s))...)
		} else {
			if fc+2*cnt > len(wd) {
				return nil, nativeUnsupported("片段超出文件范围")
			}
			u := make([]uint16, cnt)
			for j := range u {
				u[j] = uint16(le16(wd, fc+2*j))
			}
			out = append(out, utf16.Decode(u)...)
		}
	}
	return out, nil
}

// docClean 把 Word 的特殊字符换成普通文字：段落、单元格、换行；去掉域代码，只保留域结果；
// 去掉图片、脚注引用等占位符。
func docClean(rs []rune) string {
	var sb strings.Builder
	// 域：0x13 开始，0x14 分隔（之前是域代码、之后是显示结果），0x15 结束；可以嵌套
	var fields []bool // true 表示在域代码部分
	inCode := func() bool {
		for _, c := range fields {
			if c {
				return true
			}
		}
		return false
	}
	for _, r := range rs {
		switch r {
		case 0x13:
			fields = append(fields, true)
			continue
		case 0x14:
			if len(fields) > 0 {
				fields[len(fields)-1] = false
			}
			continue
		case 0x15:
			if len(fields) > 0 {
				fields = fields[:len(fields)-1]
			}
			continue
		}
		if inCode() {
			continue
		}
		switch r {
		case 0x0D, 0x0B, 0x0C, 0x0E:
			sb.WriteByte('\n')
		case 0x07: // 单元格结束
			sb.WriteByte('\t')
		case 0x1E:
			sb.WriteByte('-')
		case 0x1F, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x08, 0x00:
		case 0xA0:
			sb.WriteByte(' ')
		default:
			if r < 0x20 && r != '\t' {
				continue
			}
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
