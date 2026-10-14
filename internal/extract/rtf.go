package extract

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// rtfSkip 是不含正文的目标组（字体表、样式表、图片、域代码等），整组跳过。
// 以 \* 开头的未知目标组也跳过（RTF 规范要求读不懂的 \* 组直接忽略）。
var rtfSkip = map[string]bool{
	"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true, "pict": true,
	"objdata": true, "objclass": true, "listtable": true, "listoverridetable": true, "revtbl": true,
	"rsidtbl": true, "xmlnstbl": true, "themedata": true, "colorschememapping": true, "datastore": true,
	"latentstyles": true, "filetbl": true, "generator": true, "nonshppict": true, "shppict": true,
	"fldinst": true, "bkmkstart": true, "bkmkend": true, "sp": true, "template": true, "userprops": true,
	"docvar": true, "mmathPr": true, "pgdsctbl": true, "pn": true, "pntext": true, "shprslt": true,
	"blipuid": true, "falt": true, "panose": true, "fontemb": true, "fontfile": true, "wgrffmtfilter": true,
	"defchp": true, "defpap": true, "operator": true, "author": true, "title": true, "company": true,
	"atnid": true, "atnauthor": true,
	"atndate": true, "atnref": true, "xe": true, "tc": true, "txe": true, "bkmkcolf": true, "bkmkcoll": true,
	"passwordhash": true, "protusertbl": true, "aftnsep": true, "aftnsepc": true, "ftnsep": true, "ftnsepc": true,
}

// 在 \* 组里也要读的目标（形状和文本框里的文字）。
var rtfKeepStar = map[string]bool{"shpinst": true, "shptxt": true, "fldrslt": true}

// rtfCharsetCP 把 \fcharset 换成代码页。
var rtfCharsetCP = map[int]int{
	0: 0, 1: 0, 2: 0, // ANSI、默认、符号字体：按文档的 \ansicpg
	77: 10000, 128: 932, 129: 949, 130: 1361, 134: 936, 136: 950, 161: 1253, 162: 1254,
	163: 1258, 177: 1255, 178: 1256, 186: 1257, 204: 1251, 222: 874, 238: 1250,
}

func rtfDecoder(cp int) *encoding.Decoder {
	var e encoding.Encoding
	switch cp {
	case 936:
		e = simplifiedchinese.GBK
	case 54936:
		e = simplifiedchinese.GB18030
	case 950:
		e = traditionalchinese.Big5
	case 932:
		e = japanese.ShiftJIS
	case 949:
		e = korean.EUCKR
	case 874:
		e = charmap.Windows874
	case 1250:
		e = charmap.Windows1250
	case 1251:
		e = charmap.Windows1251
	case 1253:
		e = charmap.Windows1253
	case 1254:
		e = charmap.Windows1254
	case 1255:
		e = charmap.Windows1255
	case 1256:
		e = charmap.Windows1256
	case 1257:
		e = charmap.Windows1257
	case 1258:
		e = charmap.Windows1258
	case 10000:
		e = charmap.Macintosh
	default:
		e = charmap.Windows1252
	}
	return e.NewDecoder()
}

type rtfState struct {
	skip bool
	uc   int // \uN 之后要跳过的替代字符个数
	cp   int // 当前字体的代码页，0 表示用文档默认
}

// isRTF 判断内容是否 RTF（.doc 其实是 RTF 的情况很常见）。
func isRTF(head []byte) bool { return hasPrefixFold(head, `{\rtf`) }

// extractRTF 读取 RTF 文档的文字：正文、表格、页眉页脚、脚注、批注、文本框。
// 中文 RTF 通常用 \'hh 写 GBK 字节（按字体的 \fcharset 或文档的 \ansicpg 解码），或用 \uN 写 Unicode。
func extractRTF(p string, opt Options) doc.Result {
	raw, err := readLimited(p, opt.MaxTextBytes)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
	}
	text := rtfText(raw)
	var b doc.Builder
	para := 0
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimRight(line, " \t"); strings.TrimSpace(line) == "" {
			continue
		}
		para++
		b.Mark("第" + strconv.Itoa(para) + "段")
		b.Line(line)
	}
	return doc.Result{Status: doc.StatusOK, Text: b.String(), Locs: b.Locs}
}

func rtfText(raw []byte) string {
	var out strings.Builder
	var pend []byte    // 待解码的 \'hh 字节（双字节字符由两个 \'hh 组成）
	var pendU []uint16 // 待合并的 \uN（代理对）
	defCP := 1252
	fontCP := map[int]int{}
	st := rtfState{uc: 1}
	var stack []rtfState
	skipChars := 0 // \uN 之后还要跳过的替代字符
	// 字体表：记录字体号与字符集
	inFontTbl, fontDepth, curFont := false, 0, -1

	flushU := func() {
		if len(pendU) > 0 {
			out.WriteString(string(utf16.Decode(pendU)))
			pendU = pendU[:0]
		}
	}
	flushB := func() {
		if len(pend) > 0 {
			cp := st.cp
			if cp == 0 {
				cp = defCP
			}
			s, _ := rtfDecoder(cp).Bytes(pend)
			out.Write(s)
			pend = pend[:0]
		}
	}
	flush := func() { flushB(); flushU() }
	emit := func(s string) {
		if st.skip {
			return
		}
		flush()
		out.WriteString(s)
	}

	for i := 0; i < len(raw); {
		c := raw[i]
		switch c {
		case '{':
			flush()
			stack = append(stack, st)
			i++
			// 组的第一个控制字决定是否跳过整组
			if word, star := rtfGroupHead(raw, i); word != "" && !inFontTbl {
				switch {
				case word == "fonttbl":
					inFontTbl, fontDepth = true, len(stack)
				case rtfSkip[word], star && !rtfKeepStar[word]:
					st.skip = true
				}
			}
			continue
		case '}':
			flush()
			if len(stack) > 0 {
				if inFontTbl && len(stack) == fontDepth {
					inFontTbl = false
				}
				st = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
			skipChars = 0
			i++
			continue
		case '\r', '\n':
			i++
			continue
		case '\\':
			if i+1 >= len(raw) {
				i++
				continue
			}
			n := raw[i+1]
			switch {
			case n == '\'':
				if i+4 <= len(raw) {
					v, err := strconv.ParseUint(string(raw[i+2:i+4]), 16, 8)
					i += 4
					if err != nil {
						continue
					}
					if skipChars > 0 {
						skipChars--
						continue
					}
					if !st.skip && !inFontTbl {
						flushU()
						pend = append(pend, byte(v))
					}
				} else {
					i = len(raw)
				}
				continue
			case isAlpha(n):
				j := i + 1
				for j < len(raw) && isAlpha(raw[j]) {
					j++
				}
				word := string(raw[i+1 : j])
				hasArg, arg := false, 0
				k := j
				if k < len(raw) && (raw[k] == '-' || isDigit(raw[k])) {
					k++
					for k < len(raw) && isDigit(raw[k]) {
						k++
					}
					if v, err := strconv.Atoi(string(raw[j:k])); err == nil {
						hasArg, arg = true, v
					}
				}
				if k < len(raw) && raw[k] == ' ' {
					k++
				}
				i = k
				if inFontTbl {
					switch word {
					case "f":
						curFont = arg
					case "fcharset":
						if cp, ok := rtfCharsetCP[arg]; ok && curFont >= 0 {
							fontCP[curFont] = cp
						}
					case "cpg":
						if curFont >= 0 && arg > 0 {
							fontCP[curFont] = arg
						}
					}
					continue
				}
				if word == "bin" && hasArg && arg > 0 {
					i += arg
					continue
				}
				if skipChars > 0 && word != "u" {
					skipChars--
					continue
				}
				switch word {
				case "ansicpg":
					if arg > 0 {
						defCP = arg
					}
				case "f":
					flushB()
					st.cp = fontCP[arg]
				case "uc":
					st.uc = arg
				case "u":
					if !st.skip {
						flushB()
						if arg < 0 {
							arg += 65536
						}
						pendU = append(pendU, uint16(arg))
						if !utf16.IsSurrogate(rune(arg)) || len(pendU) >= 2 {
							flushU()
						}
					}
					skipChars = st.uc
				case "par", "line", "sect", "page", "row", "column":
					emit("\n")
				case "tab", "cell":
					emit("\t")
				case "emdash":
					emit("—")
				case "endash":
					emit("–")
				case "bullet":
					emit("•")
				case "lquote":
					emit("‘")
				case "rquote":
					emit("’")
				case "ldblquote":
					emit("“")
				case "rdblquote":
					emit("”")
				case "emspace", "enspace", "qmspace":
					emit(" ")
				}
				continue
			default:
				i += 2
				if skipChars > 0 {
					skipChars--
					continue
				}
				switch n {
				case '\\', '{', '}':
					emit(string(n))
				case '~':
					emit(" ")
				case '_':
					emit("-")
				case '\r', '\n':
					emit("\n")
				}
				continue
			}
		}
		i++
		if st.skip || inFontTbl {
			continue
		}
		if skipChars > 0 {
			skipChars--
			continue
		}
		if c >= 0x80 { // 不规范的 RTF 直接写了多字节字符
			flushU()
			pend = append(pend, c)
			continue
		}
		if c == '\t' {
			emit("\t")
			continue
		}
		flush()
		out.WriteByte(c)
	}
	flush()
	return out.String()
}

// rtfGroupHead 读取组开头的控制字（跳过 \*），返回其名称以及是否带 \*。
func rtfGroupHead(raw []byte, i int) (string, bool) {
	star := false
	for i < len(raw) && (raw[i] == '\r' || raw[i] == '\n' || raw[i] == ' ') {
		i++
	}
	if i+1 < len(raw) && raw[i] == '\\' && raw[i+1] == '*' {
		star = true
		i += 2
		for i < len(raw) && (raw[i] == '\r' || raw[i] == '\n' || raw[i] == ' ') {
			i++
		}
	}
	if i+1 >= len(raw) || raw[i] != '\\' || !isAlpha(raw[i+1]) {
		return "", star
	}
	j := i + 1
	for j < len(raw) && isAlpha(raw[j]) {
		j++
	}
	return string(raw[i+1 : j]), star
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
