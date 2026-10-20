package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bodgit/sevenzip"
	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/nwaples/rardecode/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// 容器（压缩包、邮件）：里面的每个文件、每封邮件、每个附件都是独立的搜索结果。
// 结果路径为「容器路径 › 内部路径」，嵌套容器逐层用 › 连接。

const (
	innerSep       = " › "
	maxEntryBytes  = 200 << 20
	maxChildren    = 100000
	maxTextPerMail = 32 << 20
)

// entry 是容器里的一个条目。
type entry struct {
	name      string // 容器内路径（压缩包）或主题/附件名（邮件）
	size      int64  // 解压后大小
	csize     int64  // 压缩后大小，0 表示未知
	mtime     int64
	encrypted bool
	open      func() (io.ReadCloser, error)
	// 邮件条目：已解析好的内容
	mail *mailInfo
}

type visitFn func(inner string, e *entry) (stop bool)

type walkState struct {
	opt     Options
	budget  int64 // 剩余的解压总量
	count   int
	tmpDirs []string
}

func (w *walkState) cleanup() {
	for _, d := range w.tmpDirs {
		os.RemoveAll(d)
	}
}

func (w *walkState) tempDir() (string, error) {
	d, err := os.MkdirTemp("", "zhaozhao-ct-")
	if err == nil {
		w.tmpDirs = append(w.tmpDirs, d)
	}
	return d, err
}

// isNestedContainer 判断条目是否要继续往里展开。
func isNestedContainer(name string) bool {
	e := Ext(name)
	if e == "gz" && strings.HasSuffix(strings.ToLower(name), ".tar.gz") {
		return true
	}
	if e == "pst" || e == "ost" {
		return false
	}
	return IsContainer(e) || e == "7z" || e == "rar" || e == "tar" || e == "gz" || e == "tgz"
}

func containerKind(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "tgz"
	}
	return Ext(name)
}

// archiveKinds 是按文件内容识别格式的压缩包类型。
var archiveKinds = map[string]bool{"zip": true, "7z": true, "rar": true, "tar": true, "gz": true, "tgz": true}

// sniffArchive 按文件开头的标记判断压缩包的实际格式（zip、7z、rar、gz、tar）；认不出时返回空。
// 改了扩展名的压缩包很常见（比如 zip 改名成 .rar），7-Zip 也是按内容识别的。
func sniffArchive(p string) string {
	head, err := readHead(p, 512)
	if err != nil {
		return ""
	}
	b := string(head)
	switch {
	case strings.HasPrefix(b, "PK\x03\x04"), strings.HasPrefix(b, "PK\x05\x06"), strings.HasPrefix(b, "PK\x07\x08"):
		return "zip"
	case strings.HasPrefix(b, "7z\xBC\xAF\x27\x1C"):
		return "7z"
	case strings.HasPrefix(b, "Rar!\x1A\x07"):
		return "rar"
	case strings.HasPrefix(b, "\x1F\x8B"):
		return "gz"
	case len(b) >= 262 && b[257:262] == "ustar":
		return "tar"
	}
	return ""
}

// listEntries 列出（或枚举）一个容器文件里的条目，对每个条目调用 fn。
func listEntries(p, kind string, opt Options, fn func(*entry) bool) error {
	if archiveKinds[kind] {
		if real := sniffArchive(p); real != "" && real != kind && !(real == "gz" && kind == "tgz") {
			logx.Debugf("%s 扩展名是 %s，实际是 %s 格式，按 %s 读取", logx.P(p), kind, real, real)
			kind = real
		}
	}
	switch kind {
	case "zip":
		return listZip(p, fn)
	case "tar":
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return listTar(f, fn)
	case "tgz":
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		return listTar(gz, fn)
	case "gz":
		return listGz(p, fn)
	case "7z":
		return list7z(p, fn)
	case "rar":
		return listRar(p, fn)
	case "mbox":
		return listMbox(p, opt, fn)
	}
	return fmt.Errorf("不支持的容器类型 %s", kind)
}

// decodeZipName 处理没有标记 UTF-8 的文件名：国内常见的是 GBK。
func decodeZipName(f *zip.File) string {
	n := f.Name
	if !f.NonUTF8 && utf8.ValidString(n) {
		return n
	}
	if utf8.ValidString(n) && f.NonUTF8 {
		// 声称非 UTF-8 但恰好是合法 UTF-8：多半确实是 UTF-8
		return n
	}
	if out, err := simplifiedchinese.GB18030.NewDecoder().String(n); err == nil {
		return out
	}
	return strings.ToValidUTF8(n, "?")
}

func listZip(p string, fn func(*entry) bool) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		f := f
		e := &entry{name: strings.TrimPrefix(decodeZipName(f), "/"), size: int64(f.UncompressedSize64), csize: int64(f.CompressedSize64),
			mtime: f.Modified.Unix(), encrypted: f.Flags&0x1 != 0, open: func() (io.ReadCloser, error) { return zipOpen(f) }}
		if fn(e) {
			return nil
		}
	}
	return nil
}

func listTar(r io.Reader, fn func(*entry) bool) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		e := &entry{name: h.Name, size: h.Size, mtime: h.ModTime.Unix(),
			open: func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }}
		if fn(e) { // tar 是流式的：fn 必须在返回前读完内容
			return nil
		}
	}
}

func listGz(p string, fn func(*entry) bool) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	name := gz.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	}
	st, _ := f.Stat()
	var cs int64
	if st != nil {
		cs = st.Size()
	}
	e := &entry{name: name, csize: cs, mtime: gz.ModTime.Unix(), open: func() (io.ReadCloser, error) { return io.NopCloser(gz), nil }}
	fn(e)
	return nil
}

func list7z(p string, fn func(*entry) bool) error {
	r, err := sevenzip.OpenReader(p)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(strings.ToLower(err.Error()), "encrypt") || sevenZipHeaderEncrypted(p) {
			return errEncrypted
		}
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		f := f
		e := &entry{name: f.Name, size: int64(f.UncompressedSize), mtime: f.Modified.Unix(), open: f.Open}
		if fn(e) {
			return nil
		}
	}
	return nil
}

// sevenZipHeaderEncrypted 检查 7z 的文件头是否被加密：压缩后的头部里出现 AES 编码器 ID（06 F1 07 01）。
func sevenZipHeaderEncrypted(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var sh [32]byte
	if _, err := io.ReadFull(f, sh[:]); err != nil || string(sh[:6]) != "7z\xbc\xaf\x27\x1c" {
		return false
	}
	off := int64(binary.LittleEndian.Uint64(sh[12:20]))
	size := int64(binary.LittleEndian.Uint64(sh[20:28]))
	if size <= 0 || size > 1<<20 {
		return false
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 32+off); err != nil {
		return false
	}
	return len(buf) > 0 && buf[0] == 0x17 && bytes.Contains(buf, []byte{0x06, 0xF1, 0x07, 0x01})
}

func listRar(p string, fn func(*entry) bool) error {
	r, err := rardecode.OpenReader(p)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(strings.ToLower(err.Error()), "encrypt") {
			return errEncrypted
		}
		if errors.Is(err, rardecode.ErrNoSig) {
			return errors.New("文件内容不是 RAR，也不是其他认得的压缩包格式（可能已损坏或不完整）")
		}
		return err
	}
	defer r.Close()
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "password") {
				return errEncrypted
			}
			return err
		}
		if h.IsDir {
			continue
		}
		e := &entry{name: strings.ReplaceAll(h.Name, `\`, "/"), size: h.UnPackedSize, mtime: h.ModificationTime.Unix(),
			open: func() (io.ReadCloser, error) { return io.NopCloser(r), nil }}
		if fn(e) {
			return nil
		}
	}
}

// ---- 遍历 ----

// walk 遍历容器及其嵌套容器里的全部条目。
func (w *walkState) walk(p, kind string, depth int, prefix string, visit visitFn) error {
	used := map[string]int{}
	stopped := false
	err := listEntries(p, kind, w.opt, func(e *entry) bool {
		w.count++
		if w.count > maxChildren {
			stopped = true
			return true
		}
		inner := prefix + uniqueName(used, e.name)
		if visit(inner, e) {
			stopped = true
			return true
		}
		if e.mail != nil {
			if w.walkAttachments(e.mail, inner+innerSep, depth, visit) {
				stopped = true
				return true
			}
			return false
		}
		// 嵌套容器：先把它解到临时文件再展开
		if e.mail == nil && isNestedContainer(e.name) && depth+1 < w.opt.MaxDepthOrDefault() && !e.encrypted {
			if tmp, err := w.spill(e); err == nil {
				if err := w.walk(tmp, containerKind(e.name), depth+1, inner+innerSep, visit); err == errStop {
					stopped = true
					return true
				}
			}
		}
		return false
	})
	if stopped && err == nil {
		return errStop
	}
	return err
}

var errStop = fmt.Errorf("stop")

func (o Options) MaxDepthOrDefault() int {
	if o.MaxDepth > 0 {
		return o.MaxDepth
	}
	return 3
}

func uniqueName(used map[string]int, name string) string {
	used[name]++
	if n := used[name]; n > 1 {
		return fmt.Sprintf("%s (%d)", name, n)
	}
	return name
}

// walkAttachments 遍历邮件的附件；邮件附件本身是邮件（eml/msg）时递归展开。返回 true 表示应停止。
func (w *walkState) walkAttachments(mi *mailInfo, prefix string, depth int, visit visitFn) bool {
	used := map[string]int{}
	for _, a := range mi.attachments {
		a := a
		inner := prefix + uniqueName(used, a.name)
		e := &entry{name: a.name, size: int64(len(a.data)), mtime: mi.date.Unix(),
			open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(a.data)), nil }}
		if mi.date.IsZero() {
			e.mtime = 0
		}
		kind := containerKind(a.name)
		if (kind == "eml" || kind == "msg") && depth+1 < w.opt.MaxDepthOrDefault() {
			var sub *mailInfo
			var err error
			if kind == "eml" {
				sub, err = parseEML(a.data, w.opt)
			} else {
				sub, err = parseMSG(a.data)
			}
			if err == nil {
				e.mail = sub
			}
		}
		w.count++
		if w.count > maxChildren || visit(inner, e) {
			return true
		}
		switch {
		case e.mail != nil:
			if w.walkAttachments(e.mail, inner+innerSep, depth+1, visit) {
				return true
			}
		case isNestedContainer(a.name) && depth+1 < w.opt.MaxDepthOrDefault():
			if tmp, err := w.spill(e); err == nil {
				if w.walk(tmp, kind, depth+1, inner+innerSep, visit) == errStop {
					return true
				}
			}
		}
	}
	return false
}

var (
	errEntryTooBig = errors.New("文件过大")
	errOverBudget  = errors.New("超过压缩包解压总量上限")
	errBadRatio    = errors.New("压缩比异常，疑似压缩炸弹")
)

// spill 把条目内容写到临时文件（ASCII 文件名，保留扩展名），受解压总量和压缩比限制。
// 失败时返回具体原因（不支持的压缩方式、数据损坏、超过上限等），写进失败列表。
func (w *walkState) spill(e *entry) (string, error) {
	switch {
	case e.size > maxEntryBytes:
		return "", errEntryTooBig
	case e.size > 0 && e.size > w.budget:
		return "", errOverBudget
	case e.csize > 0 && w.opt.ArchiveMaxRatio > 0 && e.size/maxInt64(e.csize, 1) > int64(w.opt.ArchiveMaxRatio) && e.size > 1<<20:
		return "", errBadRatio
	}
	rc, err := e.open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dir, err := w.tempDir()
	if err != nil {
		return "", fmt.Errorf("无法创建临时目录: %v", err)
	}
	ext := Ext(e.name)
	if strings.HasSuffix(strings.ToLower(e.name), ".tar.gz") {
		ext = "tar.gz"
	}
	name := "inner"
	if ext != "" {
		name += "." + ext
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("无法写临时文件: %v", err)
	}
	limit := int64(maxEntryBytes)
	if w.budget < limit {
		limit = w.budget
	}
	n, err := io.Copy(f, io.LimitReader(rc, limit+1))
	f.Close()
	switch {
	case err != nil:
		os.Remove(f.Name())
		return "", err
	case n > limit:
		os.Remove(f.Name())
		return "", errOverBudget
	}
	w.budget -= n
	return f.Name(), nil
}

// entryErr 把解压条目时的错误换成看得懂的说法。
func entryErr(err error) string {
	switch msg := err.Error(); {
	case errors.Is(err, zip.ErrChecksum), errors.Is(err, errZipChecksum):
		return errZipChecksum.Error()
	case errors.Is(err, zip.ErrFormat), errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(msg, "flate: corrupt"):
		return "数据已损坏（" + msg + "）"
	default:
		return msg
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- 提取 ----

func extractContainer(p, ext string, opt Options, depth int) doc.Result {
	if opt.ArchiveMaxBytes <= 0 {
		opt.ArchiveMaxBytes = 2 << 30
	}
	w := &walkState{opt: opt, budget: opt.ArchiveMaxBytes}
	defer w.cleanup()

	var res doc.Result
	res.Status = doc.StatusOK
	kind := containerKind(filepath.Base(p))
	if ext != "" && !strings.HasSuffix(strings.ToLower(p), ".tar.gz") {
		kind = ext
	}

	switch kind {
	case "eml", "msg":
		raw, err := readLimited(p, 256<<20)
		if err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: err.Error()}
		}
		var mi *mailInfo
		if kind == "eml" {
			mi, err = parseEML(raw, opt)
		} else {
			mi, err = parseMSG(raw)
		}
		if err != nil {
			return doc.Result{Status: doc.StatusFailed, Err: "邮件无法解析: " + err.Error()}
		}
		res.Text, res.Meta, res.LineBased = mi.text, mi.metaJSON(), false
		w.walkAttachments(mi, "", depth, func(inner string, e *entry) bool {
			res.Children = append(res.Children, w.childFor(inner, e))
			return false
		})
		return res
	}

	err := w.walk(p, kind, depth, "", func(inner string, e *entry) bool {
		res.Children = append(res.Children, w.childFor(inner, e))
		opt.Progress("第 %d 项", len(res.Children))
		return false
	})
	switch {
	case err == errEncrypted:
		return doc.Result{Status: doc.StatusEncrypted, Err: "压缩包已加密"}
	case err == errStop:
		res.Err = "条目过多，只索引了前 " + fmt.Sprint(maxChildren) + " 个"
	case err != nil:
		if len(res.Children) == 0 {
			return doc.Result{Status: doc.StatusFailed, Err: "无法读取: " + err.Error()}
		}
	}
	allEnc := len(res.Children) > 0
	for _, c := range res.Children {
		if c.Status != doc.StatusEncrypted {
			allEnc = false
			break
		}
	}
	if allEnc {
		res.Status, res.Err = doc.StatusEncrypted, "压缩包已加密（文件名仍可搜索）"
	}
	return res
}

// childFor 生成压缩包条目的结果。
func (w *walkState) childFor(inner string, e *entry) doc.Child {
	c := doc.Child{Inner: inner, Size: e.size, MTime: e.mtime}
	if e.mail != nil {
		c.Result = doc.Result{Status: doc.StatusOK, Text: e.mail.text, Meta: e.mail.metaJSON()}
		return c
	}
	c.Result = w.leafResult(e)
	return c
}

// leafResult 提取单个条目的内容；容器类条目只保留名字（内容由 walk 继续展开）。
func (w *walkState) leafResult(e *entry) doc.Result {
	ext := Ext(e.name)
	switch {
	case e.encrypted:
		return doc.Result{Status: doc.StatusEncrypted, Err: "已加密"}
	case isNestedContainer(e.name):
		return doc.Result{Status: doc.StatusOK}
	case e.size > maxEntryBytes:
		return doc.Result{Status: doc.StatusSkipped, Err: "文件过大，仅索引文件名"}
	case e.csize > 0 && w.opt.ArchiveMaxRatio > 0 && e.size > 1<<20 && e.size/maxInt64(e.csize, 1) > int64(w.opt.ArchiveMaxRatio):
		return doc.Result{Status: doc.StatusFailed, Err: "压缩比异常，疑似压缩炸弹，已跳过"}
	case IsImage(ext):
		return doc.Result{Status: doc.StatusSkipped, Err: "压缩包内的图片不做 OCR"}
	}
	if f, known := formats[ext]; known && f.h == hNone {
		return doc.Result{Status: doc.StatusSkipped, Err: "不支持的文件类型"}
	}
	if _, known := formats[ext]; !known && !w.opt.SniffUnknown {
		return doc.Result{Status: doc.StatusSkipped, Err: "不支持的文件类型"}
	}
	if e.size > w.budget && e.size > 0 {
		return doc.Result{Status: doc.StatusFailed, Err: "超过压缩包解压总量上限"}
	}
	tmp, err := w.spill(e)
	if err != nil {
		if msg := strings.ToLower(err.Error()); strings.Contains(msg, "password") || strings.Contains(msg, "encrypt") {
			return doc.Result{Status: doc.StatusEncrypted, Err: "已加密"}
		}
		return doc.Result{Status: doc.StatusFailed, Err: "无法从压缩包里读取：" + entryErr(err)}
	}
	defer os.Remove(tmp)
	inner := w.opt
	inner.OCR = OCRConfig{}       // 容器内部不做 OCR
	inner.LegacyMode = LegacyAuto // 容器内部的老格式文件不能单独排队转换：内置解析不了就当场转换
	r := extractLeaf(tmp, ext, inner)
	if r.Status == doc.StatusSkipped && r.Err == "" {
		r.Err = "不支持的文件类型"
	}
	return r
}

// extractLeaf 提取不是容器的文件。
func extractLeaf(p, ext string, opt Options) doc.Result {
	res := Extract(p, ext, opt)
	return res
}

// ExtractInner 把容器内的条目解出到 destDir，返回解出的文件路径（用于「打开容器内文件」）。
// inner 是 「A.zip › 合同/采购合同.docx」中容器路径之后的部分。
func ExtractInner(container, inner, destDir string, opt Options) (string, error) {
	if opt.ArchiveMaxBytes <= 0 {
		opt.ArchiveMaxBytes = 2 << 30
	}
	w := &walkState{opt: opt, budget: opt.ArchiveMaxBytes}
	defer w.cleanup()
	kind := containerKind(filepath.Base(container))
	var found *entry
	var data []byte

	target := inner
	finish := func() (string, error) {
		name := filepath.Base(strings.ReplaceAll(found.name, `\`, "/"))
		name = sanitizeFileName(name)
		out := filepath.Join(destDir, name)
		if data != nil {
			return out, os.WriteFile(out, data, 0o444)
		}
		rc, err := found.open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, io.LimitReader(rc, maxEntryBytes)); err != nil {
			f.Close()
			return "", err
		}
		return out, f.Close()
	}

	switch kind {
	case "eml", "msg":
		raw, err := readLimited(container, 256<<20)
		if err != nil {
			return "", err
		}
		var mi *mailInfo
		if kind == "eml" {
			mi, err = parseEML(raw, opt)
		} else {
			mi, err = parseMSG(raw)
		}
		if err != nil {
			return "", err
		}
		w.walkAttachments(mi, "", 0, func(in string, e *entry) bool {
			if in == target {
				found, data = grab(e)
				return true
			}
			return false
		})
		if found == nil {
			return "", fmt.Errorf("在邮件中找不到附件 %s", inner)
		}
		return finish()
	}
	err := w.walk(container, kind, 0, "", func(in string, e *entry) bool {
		if in == target {
			found, data = grab(e)
			return true
		}
		return false
	})
	if found == nil {
		if err != nil && err != errStop {
			return "", err
		}
		return "", fmt.Errorf("在压缩包中找不到 %s", inner)
	}
	return finish()
}

// grab 取出条目的名字和内容。流式容器（tar、rar）的数据必须在回调内读出。
func grab(e *entry) (*entry, []byte) {
	if e.mail != nil {
		return &entry{name: sanitizeFileName(e.mail.displayName()) + ".eml"}, e.mail.raw
	}
	cp := *e
	data := []byte{}
	if rc, err := e.open(); err == nil {
		data, _ = io.ReadAll(io.LimitReader(rc, maxEntryBytes))
		rc.Close()
		if data == nil {
			data = []byte{}
		}
	}
	return &cp, data
}

func sanitizeFileName(n string) string {
	repl := strings.NewReplacer(`<`, "_", `>`, "_", `:`, "_", `"`, "_", `/`, "_", `\`, "_", `|`, "_", `?`, "_", `*`, "_")
	n = strings.TrimSpace(repl.Replace(n))
	if n == "" || n == "." || n == ".." {
		n = "file"
	}
	if len([]rune(n)) > 120 {
		rs := []rune(n)
		ext := path.Ext(n)
		n = string(rs[:100]) + ext
	}
	return n
}
