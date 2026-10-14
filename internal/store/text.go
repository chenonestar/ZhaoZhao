package store

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/klauspost/compress/dict"
	"github.com/klauspost/compress/zstd"
)

// 提取文本存放在 texts 表里，文件通过 files.text_id 引用。内容相同的文件（哈希与大小都相同）
// 共用一份文本；没有文件引用的文本由触发器自动删除。全文索引仍然每个文件一行，不受影响。
//
// texts.flags：
//   - textLineBased：位置按行号表示；
//   - textZstd：data、locs 用 zstd 压缩，否则是 zlib（0.1.x 写入的）；
//   - textDict：压缩时用了本索引库的词典（用已提取的文本训练，中文短文本能小一成多）。
const (
	textLineBased = 1
	textZstd      = 2
	textDict      = 4
)

// Text 是提取结果的存储形式。
type Text struct {
	LineBased bool
	Text      string
	Locs      []Loc

	enc *encodedText // 预先压缩好的数据（DB.Encode，可在多个线程里并行）
}

// Loc 记录从 Offset（字符偏移）开始的内容所在位置，如「第 3 页」「Sheet1!B2」。
type Loc = doc.Loc

type encodedText struct {
	flags      int
	data, locs []byte
	codec      *codec // 压缩时用的编码器：词典变了要重新压缩
}

// codec 是本索引库的 zstd 编解码器。词典训练好后换成带词典的新编解码器；
// 解码器同时认得有词典和没有词典的数据。
type codec struct {
	enc     *zstd.Encoder
	dec     *zstd.Decoder
	hasDict bool
}

func newCodec(dct []byte) (*codec, error) {
	eo := []zstd.EOption{zstd.WithEncoderLevel(zstd.SpeedBetterCompression)}
	var do []zstd.DOption
	if len(dct) > 0 {
		eo = append(eo, zstd.WithEncoderDict(dct))
		do = append(do, zstd.WithDecoderDicts(dct))
	}
	enc, err := zstd.NewWriter(nil, eo...)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, do...)
	if err != nil {
		return nil, err
	}
	return &codec{enc: enc, dec: dec, hasDict: len(dct) > 0}, nil
}

// codec 返回当前编解码器。
func (d *DB) codec() *codec { return d.cdc.Load() }

// loadCodec 读取索引库里保存的词典（没有就不用词典）。
func (d *DB) loadCodec() error {
	var dct []byte
	if s, _ := d.GetMeta("zstd_dict"); s != "" {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("索引库里的压缩词典已损坏: %w", err)
		}
		dct = b
	}
	c, err := newCodec(dct)
	if err != nil {
		return fmt.Errorf("索引库里的压缩词典无效: %w", err)
	}
	d.cdc.Store(c)
	return nil
}

// Encode 预先压缩文本。可以在多个线程里调用；PutResult 时如果还没压缩就当场压缩。
func (d *DB) Encode(t *Text) {
	if t == nil || (t.enc != nil && t.enc.codec == d.codec()) {
		return
	}
	c := d.codec()
	e := &encodedText{flags: textZstd, codec: c}
	if t.LineBased {
		e.flags |= textLineBased
	}
	if c.hasDict {
		e.flags |= textDict
	}
	e.data = c.enc.EncodeAll([]byte(t.Text), nil)
	if len(t.Locs) > 0 {
		b, _ := json.Marshal(t.Locs)
		e.locs = c.enc.EncodeAll(b, nil)
	}
	t.enc = e
}

func (d *DB) decode(flags int, b []byte) ([]byte, error) {
	if flags&textZstd != 0 {
		return d.codec().dec.DecodeAll(b, nil)
	}
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// PutResult 记录提取结果。t 为 nil 表示无文本（失败、加密等）。
func (d *DB) PutResult(tx *sql.Tx, id int64, state int, errMsg string, hash string, ocr bool, t *Text, ftsText, nameFTS string) error {
	if _, err := tx.Exec(`DELETE FROM fts WHERE rowid=?`, id); err != nil {
		return err
	}
	n, textID := 0, int64(0)
	if t != nil {
		n = len([]rune(t.Text))
		d.Encode(t)
		res, err := tx.Exec(`INSERT INTO texts(flags,data,locs) VALUES(?,?,?)`, t.enc.flags, t.enc.data, t.enc.locs)
		if err != nil {
			return err
		}
		textID, _ = res.LastInsertId()
	}
	if _, err := tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,?)`, id, ftsText, nameFTS); err != nil {
		return err
	}
	// 换掉 text_id 后，旧文本如果没有别的文件引用，由触发器删除
	_, err := tx.Exec(`UPDATE files SET state=?, err=?, hash=CASE WHEN ?<>'' THEN ? ELSE hash END, text_len=?, ocr=?, indexed_at=?, text_id=? WHERE id=?`,
		state, errMsg, hash, hash, n, b2i(ocr), time.Now().Unix(), textID, id)
	return err
}

// GetText 读取某文件的提取文本。
func (d *DB) GetText(id int64) (*Text, error) {
	var flags int
	var data, locs []byte
	err := d.R.QueryRow(`SELECT t.flags,t.data,t.locs FROM files f JOIN texts t ON t.id=f.text_id WHERE f.id=?`, id).Scan(&flags, &data, &locs)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := d.decode(flags, data)
	if err != nil {
		return nil, err
	}
	t := &Text{LineBased: flags&textLineBased != 0, Text: string(raw)}
	if len(locs) > 0 {
		lb, err := d.decode(flags, locs)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(lb, &t.Locs); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// CopyResult 让 dst 共用 src 的提取结果（内容相同的文件，F-BG-03）：文本只存一份，
// 全文索引照样每个文件一行。
func CopyResult(tx *sql.Tx, src, dst int64, ftsText, nameFTS string) error {
	var textID int64
	if err := tx.QueryRow(`SELECT text_id FROM files WHERE id=?`, src).Scan(&textID); err != nil || textID == 0 {
		return fmt.Errorf("源文件没有提取文本")
	}
	if _, err := tx.Exec(`DELETE FROM fts WHERE rowid=?`, dst); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,?,?)`, dst, ftsText, nameFTS); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE files SET state=1, err='', text_id=?2, text_len=(SELECT text_len FROM files WHERE id=?1), ocr=(SELECT ocr FROM files WHERE id=?1), indexed_at=?3 WHERE id=?4`,
		src, textID, time.Now().Unix(), dst)
	return err
}

// FindDuplicate 查找已完成提取、内容哈希与大小都相同的另一个文件。
func (d *DB) FindDuplicate(hash string, size int64, exclude int64) (int64, error) {
	var id int64
	err := d.R.QueryRow(`SELECT id FROM files WHERE hash=? AND size=? AND state=1 AND text_id>0 AND id<>? LIMIT 1`, hash, size, exclude).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// ---- 词典训练与后台迁移（F-BG-13）----

// dictMinTexts：已提取的文本达到这么多个才训练词典（样本太少训练出的词典没有代表性）。
const dictMinTexts = 300

// HasDict 返回本索引库是否已有压缩词典。
func (d *DB) HasDict() bool { return d.codec().hasDict }

// TrainDict 用已提取的文本训练压缩词典并保存。文本还不够多时返回 false。
// 词典一旦保存就不再改变（旧数据要靠它解压）。
func (d *DB) TrainDict() (bool, error) {
	if d.HasDict() {
		return true, nil
	}
	var n int
	if err := d.R.QueryRow(`SELECT count(*) FROM texts`).Scan(&n); err != nil {
		return false, err
	}
	if n < dictMinTexts {
		return false, nil
	}
	// 随机取样：每个文本取开头最多 8 KB，共约 16 MB
	ids, err := d.randomTextIDs(2000)
	if err != nil {
		return false, err
	}
	var samples [][]byte
	for _, id := range ids {
		var flags int
		var data []byte
		if err := d.R.QueryRow(`SELECT flags, data FROM texts WHERE id=?`, id).Scan(&flags, &data); err != nil {
			continue
		}
		raw, err := d.decode(flags, data)
		if err != nil || len(raw) < 64 {
			continue
		}
		if len(raw) > 8<<10 {
			raw = raw[:8<<10]
		}
		samples = append(samples, raw)
	}
	if len(samples) < dictMinTexts/2 {
		return false, nil
	}
	dct, err := dict.BuildZstdDict(samples, dict.Options{MaxDictSize: 112 << 10, HashBytes: 6,
		ZstdDictID: 32768 + rand.Uint32N(1<<31-32768), ZstdLevel: zstd.SpeedBetterCompression})
	if err != nil {
		return false, err
	}
	c, err := newCodec(dct)
	if err != nil {
		return false, err
	}
	// 保存后再启用：先保存可保证用新词典压缩的数据永远能解开
	if err := d.SetMeta("zstd_dict", base64.StdEncoding.EncodeToString(dct)); err != nil {
		return false, err
	}
	d.cdc.Store(c)
	return true, nil
}

// MigrateStats 是后台迁移的进度。
type MigrateStats struct {
	Recompressed int   // 重新压缩的文本数
	Deduped      int   // 改为共用文本的文件数
	SavedBytes   int64 // 节省的字节数（估算）
}

// RecompressBatch 把最多 limit 个旧格式（zlib 或没用词典）的文本用当前编解码器重新压缩。
// after 是上一批最后的 id；返回本批最后的 id（0 表示已经全部完成）。
func (d *DB) RecompressBatch(after int64, limit int) (last int64, st MigrateStats, err error) {
	c := d.codec()
	want := textZstd
	if c.hasDict {
		want |= textDict
	}
	type row struct {
		id         int64
		flags      int
		data, locs []byte
	}
	rows, err := d.R.Query(`SELECT id, flags, data, locs FROM texts WHERE id>? AND (flags & 6)<>? ORDER BY id LIMIT ?`, after, want, limit)
	if err != nil {
		return 0, st, err
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.flags, &r.data, &r.locs); err != nil {
			rows.Close()
			return 0, st, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0, st, nil
	}
	type upd struct {
		id         int64
		flags      int
		data, locs []byte
	}
	var ups []upd
	for _, r := range batch {
		raw, err := d.decode(r.flags, r.data)
		if err != nil {
			continue // 解不开的旧数据保持原样
		}
		u := upd{id: r.id, flags: r.flags&textLineBased | want, data: c.enc.EncodeAll(raw, nil)}
		if len(r.locs) > 0 {
			lb, err := d.decode(r.flags, r.locs)
			if err != nil {
				continue
			}
			u.locs = c.enc.EncodeAll(lb, nil)
		}
		st.SavedBytes += int64(len(r.data)+len(r.locs)) - int64(len(u.data)+len(u.locs))
		ups = append(ups, u)
	}
	err = d.Tx(func(tx *sql.Tx) error {
		for _, u := range ups {
			// 只更新仍是旧格式的（期间可能已被重新提取）
			if _, err := tx.Exec(`UPDATE texts SET flags=?, data=?, locs=? WHERE id=? AND (flags & 6)<>?`, u.flags, u.data, u.locs, u.id, want); err != nil {
				return err
			}
		}
		return nil
	})
	st.Recompressed = len(ups)
	return batch[len(batch)-1].id, st, err
}

// DedupeBatch 让内容相同（哈希与大小都相同）但各存了一份文本的文件改为共用一份。
// 返回处理的组数（0 表示没有了）。0.1.x 复用提取结果时会复制文本，这里把它们合并。
func (d *DB) DedupeBatch(limit int) (groups int, st MigrateStats, err error) {
	rows, err := d.R.Query(`SELECT hash, size, MIN(text_id) FROM files WHERE state=1 AND hash<>'' AND text_id>0
GROUP BY hash, size HAVING COUNT(DISTINCT text_id)>1 LIMIT ?`, limit)
	if err != nil {
		return 0, st, err
	}
	type grp struct {
		hash   string
		size   int64
		textID int64
	}
	var gs []grp
	for rows.Next() {
		var g grp
		if err := rows.Scan(&g.hash, &g.size, &g.textID); err != nil {
			rows.Close()
			return 0, st, err
		}
		gs = append(gs, g)
	}
	rows.Close()
	err = d.Tx(func(tx *sql.Tx) error {
		for _, g := range gs {
			var freed sql.NullInt64
			_ = tx.QueryRow(`SELECT SUM(length(data)+ifnull(length(locs),0)) FROM texts WHERE id IN
(SELECT DISTINCT text_id FROM files WHERE hash=? AND size=? AND state=1 AND text_id>0 AND text_id<>?)`, g.hash, g.size, g.textID).Scan(&freed)
			res, err := tx.Exec(`UPDATE files SET text_id=? WHERE hash=? AND size=? AND state=1 AND text_id>0 AND text_id<>?`, g.textID, g.hash, g.size, g.textID)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			st.Deduped += int(n)
			st.SavedBytes += freed.Int64
		}
		return nil
	})
	return len(gs), st, err
}
