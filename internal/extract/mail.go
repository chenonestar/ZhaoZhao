package extract

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/richardlehane/mscfb"
	"golang.org/x/text/encoding/htmlindex"
)

type attachment struct {
	name string
	data []byte
}

// mailInfo 是解析后的一封邮件。
type mailInfo struct {
	subject, from, to, cc string
	date                  time.Time
	text                  string // 主题 + 正文
	attachments           []*attachment
	raw                   []byte
}

func (m *mailInfo) metaJSON() string {
	d := ""
	if !m.date.IsZero() {
		d = m.date.Local().Format("2006-01-02 15:04")
	}
	b, _ := json.Marshal(map[string]string{"subject": m.subject, "from": m.from, "to": m.to, "cc": m.cc, "date": d})
	return string(b)
}

func (m *mailInfo) displayName() string {
	if strings.TrimSpace(m.subject) == "" {
		return "(无主题)"
	}
	return strings.ReplaceAll(strings.TrimSpace(m.subject), "\n", " ")
}

var wordDecoder = &mime.WordDecoder{CharsetReader: func(charset string, in io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(in), nil
}}

func decodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

func addrList(h mail.Header, key string) string {
	raw := h.Get(key)
	if raw == "" {
		return ""
	}
	if list, err := h.AddressList(key); err == nil {
		var parts []string
		for _, a := range list {
			n := decodeHeader(a.Name)
			if n != "" {
				parts = append(parts, n+" <"+a.Address+">")
			} else {
				parts = append(parts, a.Address)
			}
		}
		return strings.Join(parts, ", ")
	}
	return decodeHeader(raw)
}

func decodeCharset(b []byte, charset string) string {
	if charset != "" {
		if enc, err := htmlindex.Get(charset); err == nil {
			if out, err := enc.NewDecoder().Bytes(b); err == nil {
				return string(out)
			}
		}
	}
	return DecodeText(b)
}

// decodeTransfer 按 Content-Transfer-Encoding 解码正文。
func decodeTransfer(enc string, r io.Reader) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		raw, err := io.ReadAll(io.LimitReader(r, maxTextPerMail*2))
		if err != nil {
			return nil, err
		}
		raw = bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, raw)
		out, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			out, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(string(raw), "="))
		}
		return out, err
	case "quoted-printable":
		return io.ReadAll(io.LimitReader(quotedprintable.NewReader(r), maxTextPerMail))
	}
	return io.ReadAll(io.LimitReader(r, maxEntryBytes))
}

type mailBody struct {
	plain, html strings.Builder
	atts        []*attachment
}

func parseEML(raw []byte, opt Options) (*mailInfo, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	m := &mailInfo{raw: raw}
	m.subject = decodeHeader(msg.Header.Get("Subject"))
	m.from = addrList(msg.Header, "From")
	m.to = addrList(msg.Header, "To")
	m.cc = addrList(msg.Header, "Cc")
	if d, err := msg.Header.Date(); err == nil {
		m.date = d
	}
	var body mailBody
	collectPart(textproto(msg.Header), msg.Body, &body, 0)
	text := strings.TrimSpace(body.plain.String())
	if text == "" {
		text = strings.TrimSpace(body.html.String())
	}
	m.text = strings.TrimSpace(m.subject + "\n" + plainText(text))
	m.attachments = body.atts
	return m, nil
}

// headerGetter 抽象 mail.Header 和 textproto.MIMEHeader。
type headerGetter interface{ Get(string) string }

func textproto(h mail.Header) headerGetter { return h }

func collectPart(h headerGetter, r io.Reader, out *mailBody, depth int) {
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = "text/plain"
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	if strings.HasPrefix(mt, "multipart/") && depth < 8 {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return
			}
			collectPart(p.Header, p, out, depth+1)
		}
	}
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	name = decodeHeader(name)
	enc := h.Get("Content-Transfer-Encoding")
	// multipart.Part 已经自动解码 quoted-printable，并移除了该头
	data, err := decodeTransfer(enc, r)
	if err != nil && len(data) == 0 {
		return
	}
	isAttachment := strings.EqualFold(disp, "attachment") || (name != "" && !strings.HasPrefix(mt, "text/")) || (name != "" && strings.EqualFold(disp, "inline") && !strings.HasPrefix(mt, "text/"))
	if name != "" && strings.HasPrefix(mt, "text/") && strings.EqualFold(disp, "attachment") {
		isAttachment = true
	}
	switch {
	case mt == "message/rfc822":
		if name == "" {
			name = "转发的邮件.eml"
			if mi, err := parseEML(data, Options{}); err == nil {
				name = sanitizeFileName(mi.displayName()) + ".eml"
			}
		}
		out.atts = append(out.atts, &attachment{name: name, data: data})
	case isAttachment:
		if name == "" {
			name = "附件"
		}
		out.atts = append(out.atts, &attachment{name: name, data: data})
	case mt == "text/plain":
		out.plain.WriteString(decodeCharset(data, params["charset"]))
		out.plain.WriteString("\n")
	case mt == "text/html":
		out.html.WriteString(htmlText([]byte(decodeCharset(data, params["charset"]))))
		out.html.WriteString("\n")
	}
}

// ---- mbox ----

func listMbox(p string, opt Options, fn func(*entry) bool) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	var cur bytes.Buffer
	inMsg := false
	emit := func() bool {
		if cur.Len() == 0 {
			return false
		}
		raw := append([]byte(nil), cur.Bytes()...)
		cur.Reset()
		mi, err := parseEML(raw, opt)
		if err != nil {
			return false
		}
		e := &entry{name: mi.displayName(), size: int64(len(raw)), mtime: mi.date.Unix(), mail: mi}
		if mi.date.IsZero() {
			e.mtime = 0
		}
		stop := fn(e)
		if stop {
			return true
		}
		return false
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if bytes.HasPrefix(line, []byte("From ")) && (!inMsg || cur.Len() == 0 || endsWithBlank(cur.Bytes())) {
				if inMsg && emit() {
					return nil
				}
				inMsg = true
				cur.Reset()
			} else if inMsg {
				if bytes.HasPrefix(line, []byte(">From ")) {
					line = line[1:] // mboxrd 反转义
				}
				if cur.Len() < 256<<20 {
					cur.Write(line)
				}
			}
		}
		if err != nil {
			break
		}
	}
	if inMsg {
		emit()
	}
	return nil
}

func endsWithBlank(b []byte) bool {
	return bytes.HasSuffix(b, []byte("\n\n")) || bytes.HasSuffix(b, []byte("\r\n\r\n")) || len(b) == 0
}

// ---- Outlook .msg ----

func utf16le(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:]))
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

func msgString(typ string, b []byte) string {
	switch typ {
	case "001F":
		return utf16le(b)
	case "001E":
		return strings.TrimRight(DecodeText(b), "\x00")
	}
	return ""
}

// parseMSG 解析 Outlook .msg（OLE 复合文档）。注意：尚未用大量真实样本验证。
func parseMSG(raw []byte) (m *mailInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			m, err = nil, fmt.Errorf("msg 解析异常: %v", r)
		}
	}()
	d, err := mscfb.New(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	m = &mailInfo{raw: raw}
	type att struct {
		name, short string
		data        []byte
	}
	atts := map[string]*att{}
	var attOrder []string
	var body, htmlBody []byte
	var bodyTyp string
	for e, err := d.Next(); err == nil; e, err = d.Next() {
		if e.Name == "__properties_version1.0" && len(e.Path) == 0 {
			b, _ := io.ReadAll(d)
			m.date = msgDate(b)
			continue
		}
		if !strings.HasPrefix(e.Name, "__substg1.0_") || len(e.Name) < 20 {
			continue
		}
		tag := e.Name[len("__substg1.0_"):]
		id, typ := strings.ToUpper(tag[:4]), strings.ToUpper(tag[4:8])
		data, _ := io.ReadAll(d)
		if len(e.Path) == 0 {
			switch id {
			case "0037":
				m.subject = msgString(typ, data)
			case "1000":
				body, bodyTyp = data, typ
			case "1013":
				htmlBody = data
			case "0C1A":
				if m.from == "" {
					m.from = msgString(typ, data)
				}
			case "5D01", "0C1F":
				if a := msgString(typ, data); a != "" {
					if m.from != "" && !strings.Contains(m.from, a) {
						m.from += " <" + a + ">"
					} else if m.from == "" {
						m.from = a
					}
				}
			case "0E04":
				m.to = msgString(typ, data)
			case "0E03":
				m.cc = msgString(typ, data)
			}
			continue
		}
		if strings.HasPrefix(e.Path[0], "__attach_version1.0_") {
			a := atts[e.Path[0]]
			if a == nil {
				a = &att{}
				atts[e.Path[0]] = a
				attOrder = append(attOrder, e.Path[0])
			}
			switch id {
			case "3707":
				a.name = msgString(typ, data)
			case "3704":
				a.short = msgString(typ, data)
			case "3701":
				a.data = data
			}
		}
	}
	text := ""
	if body != nil {
		text = msgString(bodyTyp, body)
	}
	if strings.TrimSpace(text) == "" && htmlBody != nil {
		text = htmlText(htmlBody)
	}
	m.text = strings.TrimSpace(m.subject + "\n" + plainText(text))
	for _, k := range attOrder {
		a := atts[k]
		if len(a.data) == 0 {
			continue
		}
		n := a.name
		if n == "" {
			n = a.short
		}
		if n == "" {
			n = "附件"
		}
		m.attachments = append(m.attachments, &attachment{name: n, data: a.data})
	}
	return m, nil
}

// msgDate 从 __properties_version1.0 里取发送时间（PidTagClientSubmitTime，其次 MessageDeliveryTime）。
func msgDate(b []byte) time.Time {
	if len(b) < 32 {
		return time.Time{}
	}
	var sub, del time.Time
	for off := 32; off+16 <= len(b); off += 16 {
		typ := binary.LittleEndian.Uint16(b[off:])
		id := binary.LittleEndian.Uint16(b[off+2:])
		if typ != 0x0040 {
			continue
		}
		ft := binary.LittleEndian.Uint64(b[off+8:])
		t := time.Unix(int64(ft/10000000)-11644473600, 0)
		switch id {
		case 0x0039:
			sub = t
		case 0x0E06:
			del = t
		}
	}
	if !sub.IsZero() {
		return sub
	}
	return del
}
