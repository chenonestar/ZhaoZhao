package extract

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// PowerPoint 97-2003（.ppt/.pps/.pot）以及与之兼容的 WPS 演示（.dps）记录类型，见 MS-PPT。
const (
	pptDocument        = 0x03E8
	pptSlide           = 0x03EE
	pptSlideAtom       = 0x03EF
	pptNotes           = 0x03F0
	pptSlidePersist    = 0x03F3
	pptMainMaster      = 0x03F8
	pptDrawing         = 0x040C
	pptOutlineTextRef  = 0x0F9E
	pptTextChars       = 0x0FA0
	pptTextBytes       = 0x0FA8
	pptSlideList       = 0x0FF0
	pptUserEdit        = 0x0FF5
	pptCurrentUser     = 0x0FF6
	pptPersistDir      = 0x1772
	pptCryptSession    = 0x2F14
	pptClientTextbox   = 0xF00D
	pptTokenEncrypted  = 0xF3D1C4DF
	pptMaxRecordsWalk  = 1 << 22
	pptMaxEditsInChain = 4096
)

type pptRec struct {
	ver, inst, typ int
	data           []byte
}

// pptRecords 把一段字节切分成相邻的记录。
func pptRecords(b []byte) []pptRec {
	var out []pptRec
	for off := 0; off+8 <= len(b) && len(out) < pptMaxRecordsWalk; {
		vi, typ, n := le16(b, off), le16(b, off+2), le32(b, off+4)
		if n < 0 || off+8+n > len(b) {
			n = len(b) - off - 8 // 最后一条记录被截断：尽量读
		}
		out = append(out, pptRec{ver: vi & 0xF, inst: vi >> 4, typ: typ, data: b[off+8 : off+8+n]})
		off += 8 + n
	}
	return out
}

// pptRecordAt 读取偏移 off 处的一条记录。
func pptRecordAt(b []byte, off int) (pptRec, bool) {
	if off < 0 || off+8 > len(b) {
		return pptRec{}, false
	}
	n := le32(b, off+4)
	if n < 0 || off+8+n > len(b) {
		return pptRec{}, false
	}
	vi := le16(b, off)
	return pptRec{ver: vi & 0xF, inst: vi >> 4, typ: le16(b, off+2), data: b[off+8 : off+8+n]}, true
}

func pptText(r pptRec) string {
	var s string
	if r.typ == pptTextChars {
		u := make([]uint16, len(r.data)/2)
		for i := range u {
			u[i] = uint16(le16(r.data, 2*i))
		}
		s = string(utf16.Decode(u))
	} else {
		// TextBytesAtom：每个字节是 UTF-16 字符的低字节
		rs := make([]rune, len(r.data))
		for i, c := range r.data {
			rs[i] = rune(c)
		}
		s = string(rs)
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == 0x0B:
			return '\n'
		case r == '\t' || r == '\n':
			return r
		case r < 0x20 || r == 0xFFFE || r == 0xFFFF:
			return -1
		}
		return r
	}, s)
}

type pptSlideRef struct {
	persist, id int
	texts       []string // SlideListWithText 里属于这张幻灯片的文字（占位符文字）
}

type pptFile struct {
	b       []byte
	persist map[int]int // persist id → 流内偏移
}

// extractPptNative 直接读取演示文稿中的文字，不启动 LibreOffice。
// 依据 MS-PPT：Current User 流指向最近一次保存的 UserEditAtom，沿 offsetLastEdit 链读出全部
// PersistDirectoryAtom，得到每个对象的位置；DocumentContainer 里的 SlideListWithText 给出幻灯片和备注的顺序。
// 只读当前有效的幻灯片（快速保存留下的旧版本不读），不读母版里的「单击此处编辑母版标题样式」。
func extractPptNative(p string) (doc.Result, error) {
	st, all, err := oleStreams(p, "PowerPoint Document", "Current User")
	if err != nil {
		return doc.Result{}, err
	}
	if all["EncryptedSummary"] {
		return doc.Result{}, errNativeEncrypted
	}
	b := st["PowerPoint Document"]
	if len(b) < 8 {
		return doc.Result{}, nativeUnsupported("没有 PowerPoint Document 流")
	}
	f := &pptFile{b: b, persist: map[int]int{}}
	docOff, err := f.load(st["Current User"])
	if err == errNativeEncrypted {
		return doc.Result{}, err
	}
	dc, ok := pptRecordAt(b, docOff)
	if err != nil || !ok || dc.typ != pptDocument {
		return pptScan(b)
	}
	var slides, notes []pptSlideRef
	for _, r := range pptRecords(dc.data) {
		if r.typ != pptSlideList {
			continue
		}
		switch r.inst {
		case 0:
			slides = pptSlideRefs(r)
		case 2:
			notes = pptSlideRefs(r)
		}
	}
	notesByID := map[int]pptSlideRef{}
	for _, n := range notes {
		notesByID[n.id] = n
	}
	var bd doc.Builder
	write := func(label, s string) {
		for _, line := range strings.Split(s, "\n") {
			// 只有「*」的行是页码、日期等域的占位符
			if line = strings.TrimRight(line, " \t"); strings.TrimSpace(line) == "" || strings.TrimSpace(line) == "*" {
				continue
			}
			bd.Mark(label)
			bd.Line(line)
		}
	}
	for i, s := range slides {
		label := "幻灯片 " + strconv.Itoa(i+1)
		notesID := 0
		for _, t := range f.slideTexts(s, pptSlide, &notesID) {
			write(label, t)
		}
		if n, ok := notesByID[notesID]; ok && notesID != 0 {
			for _, t := range f.slideTexts(n, pptNotes, nil) {
				write(label+" 备注", t)
			}
		}
	}
	return doc.Result{Status: doc.StatusOK, Text: bd.String(), Locs: bd.Locs}, nil
}

// load 读取 Current User 和 UserEditAtom 链，建立 persist 目录，返回 DocumentContainer 的偏移。
func (f *pptFile) load(cu []byte) (int, error) {
	if len(cu) < 20 || le16(cu, 2) != pptCurrentUser {
		return 0, nativeUnsupported("Current User 流异常")
	}
	if uint32(le32(cu, 12)) == pptTokenEncrypted {
		return 0, errNativeEncrypted
	}
	off := le32(cu, 16)
	var edits []pptRec
	seen := map[int]bool{}
	for off > 0 && !seen[off] && len(edits) < pptMaxEditsInChain {
		seen[off] = true
		r, ok := pptRecordAt(f.b, off)
		if !ok || r.typ != pptUserEdit || len(r.data) < 28 {
			return 0, nativeUnsupported("UserEditAtom 异常")
		}
		if len(r.data) >= 32 && le32(r.data, 28) != 0 { // encryptSessionPersistIdRef
			return 0, errNativeEncrypted
		}
		edits = append(edits, r)
		off = le32(r.data, 8)
	}
	if len(edits) == 0 {
		return 0, nativeUnsupported("没有 UserEditAtom")
	}
	// 从最早的一次保存开始读，后保存的覆盖先保存的
	for i := len(edits) - 1; i >= 0; i-- {
		r, ok := pptRecordAt(f.b, le32(edits[i].data, 12))
		if !ok || r.typ != pptPersistDir {
			return 0, nativeUnsupported("PersistDirectoryAtom 异常")
		}
		d := r.data
		for j := 0; j+4 <= len(d); {
			v := le32(d, j)
			id, n := v&0xFFFFF, v>>20
			j += 4
			for k := 0; k < n && j+4 <= len(d); k++ {
				f.persist[id+k] = le32(d, j)
				j += 4
			}
		}
	}
	docOff, ok := f.persist[le32(edits[0].data, 16)]
	if !ok {
		return 0, nativeUnsupported("找不到文档记录")
	}
	if r, ok := pptRecordAt(f.b, docOff); ok && r.typ == pptCryptSession {
		return 0, errNativeEncrypted
	}
	return docOff, nil
}

func pptSlideRefs(r pptRec) []pptSlideRef {
	var out []pptSlideRef
	for _, c := range pptRecords(r.data) {
		switch c.typ {
		case pptSlidePersist:
			if len(c.data) >= 16 {
				out = append(out, pptSlideRef{persist: le32(c.data, 0), id: le32(c.data, 12)})
			}
		case pptTextChars, pptTextBytes:
			if len(out) > 0 {
				last := &out[len(out)-1]
				last.texts = append(last.texts, pptText(c))
			}
		}
	}
	return out
}

// slideTexts 按形状顺序返回一张幻灯片（或备注页）的文字。形状里的 OutlineTextRefAtom 引用
// SlideListWithText 里的文字；没有被引用到的占位符文字放在最后。notesID 非空时返回这张幻灯片对应的备注 id。
func (f *pptFile) slideTexts(s pptSlideRef, want int, notesID *int) []string {
	var out []string
	used := make([]bool, len(s.texts))
	if off, ok := f.persist[s.persist]; ok {
		if sc, ok := pptRecordAt(f.b, off); ok && sc.typ == want {
			for _, r := range pptRecords(sc.data) {
				switch {
				case r.typ == pptSlideAtom && notesID != nil && len(r.data) >= 20:
					*notesID = le32(r.data, 16)
				case r.typ == pptDrawing:
					f.drawingTexts(r, s.texts, used, &out, 0)
				}
			}
		}
	}
	for i, t := range s.texts {
		if !used[i] {
			out = append(out, t)
		}
	}
	return out
}

func (f *pptFile) drawingTexts(r pptRec, refs []string, used []bool, out *[]string, depth int) {
	if depth > 32 {
		return
	}
	for _, c := range pptRecords(r.data) {
		switch {
		case c.typ == pptTextChars || c.typ == pptTextBytes:
			if r.typ == pptClientTextbox {
				*out = append(*out, pptText(c))
			}
		case c.typ == pptOutlineTextRef && len(c.data) >= 4:
			if i := le32(c.data, 0); i < len(refs) && !used[i] {
				used[i] = true
				*out = append(*out, refs[i])
			}
		case c.ver == 0xF && c.typ != pptMainMaster:
			f.drawingTexts(c, refs, used, out, depth+1)
		}
	}
}

// pptScan 在找不到对象目录时（个别软件生成的文件没有 Current User 流等）按顺序扫描全部记录取文字，
// 跳过母版。可能包含快速保存留下的旧版本文字。
func pptScan(b []byte) (doc.Result, error) {
	var bd doc.Builder
	last := ""
	var walk func(recs []pptRec, depth int)
	walk = func(recs []pptRec, depth int) {
		for _, r := range recs {
			switch {
			case r.typ == pptTextChars || r.typ == pptTextBytes:
				t := pptText(r)
				if t == last {
					continue
				}
				last = t
				for _, line := range strings.Split(t, "\n") {
					if s := strings.TrimSpace(line); s != "" && s != "*" {
						bd.Mark("演示文稿")
						bd.Line(strings.TrimRight(line, " \t"))
					}
				}
			case r.typ == pptCryptSession:
				return
			case r.ver == 0xF && r.typ != pptMainMaster && !(r.typ == pptSlideList && r.inst == 1) && depth < 32:
				walk(pptRecords(r.data), depth+1)
			}
		}
	}
	walk(pptRecords(b), 0)
	if bd.Len() == 0 {
		return doc.Result{}, nativeUnsupported("没有找到文字记录")
	}
	return doc.Result{Status: doc.StatusOK, Text: bd.String(), Locs: bd.Locs}, nil
}
