// Package store 封装 SQLite：卷、索引根、文件元数据、提取文本与全文索引。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// 文件内容处理状态。
const (
	StatePending   = 0 // 待提取
	StateDone      = 1 // 已提取
	StateFailed    = 2 // 失败（原因见 Err）
	StateEncrypted = 3 // 已加密，已跳过
	StateMetaOnly  = 4 // 只索引文件名（超大、不支持、无文字等）
)

// SchemaVersion 是当前索引库格式版本。
//   - 2：内容相同的文件共用一份提取文本（files.text_id → texts.id），文本改用 zstd 压缩。
const SchemaVersion = 2

type DB struct {
	W    *sql.DB // 唯一的写连接
	R    *sql.DB // 读连接池
	Path string

	cdc atomic.Pointer[codec] // 提取文本的编解码器（训练出词典后替换）
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS volumes(
  id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '', serial TEXT NOT NULL DEFAULT '',
  guid TEXT NOT NULL DEFAULT '', capacity INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT 'local', last_mount TEXT NOT NULL DEFAULT '', last_seen INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS roots(
  id INTEGER PRIMARY KEY, volume_id TEXT NOT NULL, rel_path TEXT NOT NULL,
  paused INTEGER NOT NULL DEFAULT 0, opts TEXT NOT NULL DEFAULT '{}', added_at INTEGER NOT NULL,
  last_scan INTEGER NOT NULL DEFAULT 0, UNIQUE(volume_id, rel_path));
CREATE TABLE IF NOT EXISTS files(
  id INTEGER PRIMARY KEY, root_id INTEGER NOT NULL, volume_id TEXT NOT NULL,
  rel_path TEXT NOT NULL, inner TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL, ext TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0, mtime INTEGER NOT NULL DEFAULT 0,
  state INTEGER NOT NULL DEFAULT 0, err TEXT NOT NULL DEFAULT '', hash TEXT NOT NULL DEFAULT '',
  text_len INTEGER NOT NULL DEFAULT 0, ocr INTEGER NOT NULL DEFAULT 0, meta TEXT NOT NULL DEFAULT '',
  prio INTEGER NOT NULL DEFAULT 1, indexed_at INTEGER NOT NULL DEFAULT 0, text_id INTEGER NOT NULL DEFAULT 0,
  UNIQUE(volume_id, rel_path, inner));
CREATE INDEX IF NOT EXISTS files_root ON files(root_id);
CREATE INDEX IF NOT EXISTS files_state ON files(state, prio, id);
CREATE INDEX IF NOT EXISTS files_hash ON files(hash) WHERE hash <> '';
CREATE INDEX IF NOT EXISTS files_stats ON files(root_id, state, prio, ocr);
CREATE INDEX IF NOT EXISTS files_queue ON files(prio, mtime) WHERE state=0;
CREATE TABLE IF NOT EXISTS texts(id INTEGER PRIMARY KEY, flags INTEGER NOT NULL DEFAULT 0, data BLOB NOT NULL, locs BLOB);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(c, n, content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 0');
CREATE VIRTUAL TABLE IF NOT EXISTS fts_v USING fts5vocab(fts, 'row');
`

// schemaV2 在升级（schemaUpgrades）之后执行：依赖 files.text_id 的索引和触发器。
// 没有文件引用的文本自动删除：文件被删除、重新提取、内容变化时都不必另外处理文本。
const schemaV2 = `
CREATE INDEX IF NOT EXISTS files_text ON files(text_id) WHERE text_id>0;
CREATE TRIGGER IF NOT EXISTS files_text_del AFTER DELETE ON files WHEN OLD.text_id>0 BEGIN
  DELETE FROM texts WHERE id=OLD.text_id AND NOT EXISTS (SELECT 1 FROM files WHERE text_id=OLD.text_id);
END;
CREATE TRIGGER IF NOT EXISTS files_text_upd AFTER UPDATE OF text_id ON files WHEN OLD.text_id>0 AND OLD.text_id<>NEW.text_id BEGIN
  DELETE FROM texts WHERE id=OLD.text_id AND NOT EXISTS (SELECT 1 FROM files WHERE text_id=OLD.text_id);
END;
`

// Open 打开（必要时创建）数据目录下的索引库。
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "index.db")
	// journal_size_limit：WAL 文件在检查点之后截短到 64 MB 以内，不会一直占着几百 MB
	dsn := "file:" + filepath.ToSlash(p) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)&_pragma=journal_size_limit(67108864)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	// 新建的索引库用增量回收：删除内容后空出的空间可以在空闲时逐步还给磁盘（只能在建表前设置）
	var tables int
	_ = w.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&tables)
	if tables == 0 {
		_, _ = w.Exec(`PRAGMA auto_vacuum=INCREMENTAL`)
	}
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("初始化索引库失败: %w", err)
	}
	if err := upgradeSchema(w); err != nil {
		w.Close()
		return nil, fmt.Errorf("升级索引库失败: %w", err)
	}
	if _, err := w.Exec(schemaV2); err != nil {
		w.Close()
		return nil, fmt.Errorf("初始化索引库失败: %w", err)
	}
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	db := &DB{W: w, R: r, Path: p}
	if err := db.loadCodec(); err != nil {
		db.Close()
		return nil, err
	}
	_ = db.SetMeta("schema_version", fmt.Sprint(SchemaVersion))
	return db, nil
}

func (d *DB) Close() error {
	d.R.Close()
	return d.W.Close()
}

// Check 做一次完整性检查，用于发现索引损坏（F-BG-12）。
func (d *DB) Check() error {
	var s string
	if err := d.R.QueryRow(`PRAGMA quick_check`).Scan(&s); err != nil {
		return err
	}
	if s != "ok" {
		return fmt.Errorf("索引库损坏: %s", s)
	}
	return nil
}

func (d *DB) GetMeta(k string) (string, error) {
	var v string
	err := d.R.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) SetMeta(k, v string) error {
	_, err := d.W.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// GetJSON / SetJSON 读写保存为 JSON 的设置。
func (d *DB) GetJSON(k string, out any) (bool, error) {
	v, err := d.GetMeta("json:" + k)
	if err != nil || v == "" {
		return false, err
	}
	return true, json.Unmarshal([]byte(v), out)
}

func (d *DB) SetJSON(k string, in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return d.SetMeta("json:"+k, string(b))
}

// Tx 在写连接上执行事务。
func (d *DB) Tx(fn func(tx *sql.Tx) error) error {
	tx, err := d.W.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- 卷 ----

type Volume struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Serial    string `json:"serial"`
	GUID      string `json:"guid"`
	Capacity  int64  `json:"capacity"`
	Kind      string `json:"kind"` // local | removable | network
	LastMount string `json:"lastMount"`
	LastSeen  int64  `json:"lastSeen"`
}

func (d *DB) UpsertVolume(v Volume) error {
	_, err := d.W.Exec(`INSERT INTO volumes(id,label,serial,guid,capacity,kind,last_mount,last_seen) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET label=excluded.label, serial=excluded.serial, guid=excluded.guid,
capacity=excluded.capacity, kind=excluded.kind, last_mount=excluded.last_mount, last_seen=excluded.last_seen`,
		v.ID, v.Label, v.Serial, v.GUID, v.Capacity, v.Kind, v.LastMount, v.LastSeen)
	return err
}

func (d *DB) TouchVolume(id, mount string, t int64) error {
	_, err := d.W.Exec(`UPDATE volumes SET last_mount=?, last_seen=? WHERE id=?`, mount, t, id)
	return err
}

func (d *DB) GetVolume(id string) (*Volume, error) {
	var v Volume
	err := d.R.QueryRow(`SELECT id,label,serial,guid,capacity,kind,last_mount,last_seen FROM volumes WHERE id=?`, id).
		Scan(&v.ID, &v.Label, &v.Serial, &v.GUID, &v.Capacity, &v.Kind, &v.LastMount, &v.LastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &v, err
}

func (d *DB) ListVolumes() ([]Volume, error) {
	rows, err := d.R.Query(`SELECT id,label,serial,guid,capacity,kind,last_mount,last_seen FROM volumes ORDER BY label,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ID, &v.Label, &v.Serial, &v.GUID, &v.Capacity, &v.Kind, &v.LastMount, &v.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- 索引根 ----

type Root struct {
	ID       int64           `json:"id"`
	VolumeID string          `json:"volumeId"`
	RelPath  string          `json:"relPath"`
	Paused   bool            `json:"paused"`
	Opts     json.RawMessage `json:"opts"`
	AddedAt  int64           `json:"addedAt"`
	LastScan int64           `json:"lastScan"`
}

func (d *DB) AddRoot(volumeID, rel string) (int64, error) {
	rel = strings.Trim(rel, "/")
	_, err := d.W.Exec(`INSERT OR IGNORE INTO roots(volume_id,rel_path,added_at) VALUES(?,?,?)`, volumeID, rel, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.W.QueryRow(`SELECT id FROM roots WHERE volume_id=? AND rel_path=?`, volumeID, rel).Scan(&id)
	return id, err
}

func (d *DB) ListRoots() ([]Root, error) {
	rows, err := d.R.Query(`SELECT id,volume_id,rel_path,paused,opts,added_at,last_scan FROM roots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Root
	for rows.Next() {
		var r Root
		var p int
		var opts string
		if err := rows.Scan(&r.ID, &r.VolumeID, &r.RelPath, &p, &opts, &r.AddedAt, &r.LastScan); err != nil {
			return nil, err
		}
		r.Paused = p != 0
		r.Opts = json.RawMessage(opts)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) GetRoot(id int64) (*Root, error) {
	rs, err := d.ListRoots()
	if err != nil {
		return nil, err
	}
	for i := range rs {
		if rs[i].ID == id {
			return &rs[i], nil
		}
	}
	return nil, nil
}

func (d *DB) SetRootPaused(id int64, paused bool) error {
	_, err := d.W.Exec(`UPDATE roots SET paused=? WHERE id=?`, b2i(paused), id)
	return err
}

func (d *DB) SetRootOpts(id int64, opts json.RawMessage) error {
	_, err := d.W.Exec(`UPDATE roots SET opts=? WHERE id=?`, string(opts), id)
	return err
}

func (d *DB) SetRootScanned(id int64, t int64) error {
	_, err := d.W.Exec(`UPDATE roots SET last_scan=? WHERE id=?`, t, id)
	return err
}

// RemoveRoot 删除索引根及其全部文件、提取文本与全文索引。
func (d *DB) RemoveRoot(id int64) error {
	return d.Tx(func(tx *sql.Tx) error {
		if err := deleteWhere(tx, `root_id=?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM roots WHERE id=?`, id)
		return err
	})
}

// RemoveVolume 删除某个卷的全部索引。
func (d *DB) RemoveVolume(id string) error {
	return d.Tx(func(tx *sql.Tx) error {
		if err := deleteWhere(tx, `volume_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM roots WHERE volume_id=?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM volumes WHERE id=?`, id)
		return err
	})
}

func deleteWhere(tx *sql.Tx, cond string, arg any) error {
	for _, q := range []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM files WHERE ` + cond + `)`,
		`DELETE FROM files WHERE ` + cond, // 文本由触发器删除
	} {
		if _, err := tx.Exec(q, arg); err != nil {
			return err
		}
	}
	return nil
}

// ---- 文件 ----

type File struct {
	ID       int64  `json:"id"`
	RootID   int64  `json:"rootId"`
	VolumeID string `json:"volumeId"`
	RelPath  string `json:"relPath"`
	Inner    string `json:"inner"`
	Name     string `json:"name"`
	Ext      string `json:"ext"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"`
	State    int    `json:"state"`
	Err      string `json:"err"`
	Hash     string `json:"hash"`
	TextLen  int    `json:"textLen"`
	OCR      bool   `json:"ocr"`
	Meta     string `json:"meta"`
	Prio     int    `json:"prio"`
}

const fileCols = `id,root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,hash,text_len,ocr,meta,prio`

type scanner interface{ Scan(...any) error }

func scanFile(s scanner) (File, error) {
	var f File
	var ocr int
	err := s.Scan(&f.ID, &f.RootID, &f.VolumeID, &f.RelPath, &f.Inner, &f.Name, &f.Ext, &f.Kind, &f.Size, &f.MTime,
		&f.State, &f.Err, &f.Hash, &f.TextLen, &ocr, &f.Meta, &f.Prio)
	f.OCR = ocr != 0
	return f, err
}

// FileSig 是扫描对账时使用的轻量记录。
type FileSig struct {
	ID    int64
	Size  int64
	MTime int64
	State int
}

// RootSigs 返回某索引根下所有顶层文件（非容器内部条目）的签名，键为 rel_path。
func (d *DB) RootSigs(rootID int64) (map[string]FileSig, error) {
	rows, err := d.R.Query(`SELECT id,rel_path,size,mtime,state FROM files WHERE root_id=? AND inner=''`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]FileSig{}
	for rows.Next() {
		var s FileSig
		var p string
		if err := rows.Scan(&s.ID, &p, &s.Size, &s.MTime, &s.State); err != nil {
			return nil, err
		}
		m[p] = s
	}
	return m, rows.Err()
}

// ErrRootGone 表示文件所属的索引根已被删除：正在进行的扫描不应再写入。
var ErrRootGone = errors.New("索引根已被删除")

// UpsertFile 写入文件元数据与文件名索引。nameFTS 为已分词的文件名。
// 元数据发生变化时状态重置为 f.State（通常是待提取），并清除旧的提取文本。
func UpsertFile(tx *sql.Tx, f *File, nameFTS string) error {
	// 写入是串行的：在同一事务里确认索引根仍然存在，删除索引根之后就不会再冒出它的文件
	var one int
	if err := tx.QueryRow(`SELECT 1 FROM roots WHERE id=?`, f.RootID).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return ErrRootGone
		}
		return err
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM files WHERE volume_id=? AND rel_path=? AND inner=?`, f.VolumeID, f.RelPath, f.Inner).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		res, err := tx.Exec(`INSERT INTO files(root_id,volume_id,rel_path,inner,name,ext,kind,size,mtime,state,err,hash,text_len,ocr,meta,prio,indexed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,0,?,?,0)`,
			f.RootID, f.VolumeID, f.RelPath, f.Inner, f.Name, f.Ext, f.Kind, f.Size, f.MTime, f.State, f.Err, f.Hash, f.Meta, f.Prio)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(`DELETE FROM fts WHERE rowid=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE files SET root_id=?,name=?,ext=?,kind=?,size=?,mtime=?,state=?,err=?,hash=?,text_len=0,ocr=0,meta=?,prio=?,indexed_at=0,text_id=0 WHERE id=?`,
			f.RootID, f.Name, f.Ext, f.Kind, f.Size, f.MTime, f.State, f.Err, f.Hash, f.Meta, f.Prio, id); err != nil {
			return err
		}
	}
	f.ID = id
	_, err = tx.Exec(`INSERT INTO fts(rowid,c,n) VALUES(?,'',?)`, id, nameFTS)
	return err
}

// DeleteFiles 删除文件及其文本与索引；同时删除以它们为容器的内部条目。
// SetSig 更新文件记录的大小和修改时间（提取时发现与扫描记录不一致时，以实际读到的为准）。
func (d *DB) SetSig(id, size, mtime int64) error {
	_, err := d.W.Exec(`UPDATE files SET size=?, mtime=? WHERE id=?`, size, mtime, id)
	return err
}

func (d *DB) DeleteFiles(ids []int64) error {
	return d.Tx(func(tx *sql.Tx) error { return DeleteFilesTx(tx, ids) })
}

func DeleteFilesTx(tx *sql.Tx, ids []int64) error {
	for _, id := range ids {
		var vol, rel, inner string
		if err := tx.QueryRow(`SELECT volume_id,rel_path,inner FROM files WHERE id=?`, id).Scan(&vol, &rel, &inner); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return err
		}
		cond := `id=?`
		var arg any = id
		if inner == "" {
			// 连同容器内部条目一并删除
			if err := deleteChildren(tx, vol, rel); err != nil {
				return err
			}
		}
		if err := deleteWhere(tx, cond, arg); err != nil {
			return err
		}
	}
	return nil
}

func deleteChildren(tx *sql.Tx, vol, rel string) error {
	for _, q := range []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM files WHERE volume_id=?1 AND rel_path=?2 AND inner<>'')`,
		`DELETE FROM files WHERE volume_id=?1 AND rel_path=?2 AND inner<>''`,
	} {
		if _, err := tx.Exec(q, vol, rel); err != nil {
			return err
		}
	}
	return nil
}

// DeleteOrphans 删除不属于任何索引根的文件（旧版本删除索引根时可能残留），返回删除数量。
func (d *DB) DeleteOrphans() (int64, error) {
	var n int64
	if err := d.R.QueryRow(`SELECT COUNT(*) FROM files WHERE root_id NOT IN (SELECT id FROM roots)`).Scan(&n); err != nil || n == 0 {
		return 0, err
	}
	return n, d.Tx(func(tx *sql.Tx) error { return deleteWhere(tx, `root_id NOT IN (SELECT id FROM roots) AND 1=?`, 1) })
}

// ReplaceChildren 删除容器的全部内部条目（重新提取前调用）。
func ReplaceChildren(tx *sql.Tx, vol, rel string) error { return deleteChildren(tx, vol, rel) }

func (d *DB) GetFile(id int64) (*File, error) {
	f, err := scanFile(d.R.QueryRow(`SELECT `+fileCols+` FROM files WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &f, err
}

// NextPending 取出待提取的文件，按优先级（数值小者优先）、id 排序。
// 已暂停的索引根、skipVols 中的卷（离线）、exclude 中的文件（处理中）会被跳过。
// NextPending 取出待处理的文件：只取 prios 列出的优先级，同一优先级里最近修改的在前
// （用户最常找的是近期的文件，它们最先可以按内容搜到）。
func (d *DB) NextPending(limit int, prios []int, skipVols []string, exclude []int64) ([]File, error) {
	if len(prios) == 0 {
		return nil, nil
	}
	q := `SELECT ` + fileCols + ` FROM files WHERE state=0 AND prio IN (` + strings.TrimSuffix(strings.Repeat("?,", len(prios)), ",") + `) AND root_id NOT IN (SELECT id FROM roots WHERE paused=1)`
	var args []any
	for _, p := range prios {
		args = append(args, p)
	}
	if len(skipVols) > 0 {
		q += ` AND volume_id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(skipVols)), ",") + `)`
		for _, v := range skipVols {
			args = append(args, v)
		}
	}
	if len(exclude) > 0 {
		q += ` AND id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(exclude)), ",") + `)`
		for _, v := range exclude {
			args = append(args, v)
		}
	}
	q += ` ORDER BY prio, mtime DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.R.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteSubtree 删除某路径（文件或目录）及其下的全部条目，返回删除数量。
func (d *DB) DeleteSubtree(volumeID, rel string) (int, error) {
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(rel)
	rows, err := d.W.Query(`SELECT id FROM files WHERE volume_id=? AND inner='' AND (rel_path=? OR rel_path LIKE ? ESCAPE '\')`, volumeID, rel, esc+"/%")
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}
	return len(ids), d.DeleteFiles(ids)
}

// PendingCount 返回某优先级以上（数值大于 prio）的待处理数量，用于显示 OCR 队列长度。
func (d *DB) PendingByPrio(prio int) (int64, error) {
	var n int64
	err := d.R.QueryRow(`SELECT COUNT(*) FROM files WHERE state=0 AND prio=?`, prio).Scan(&n)
	return n, err
}

// ---- 状态 ----

type Counts struct {
	Total, Pending, Done, Failed, Encrypted, MetaOnly, OCR int64
}

func (d *DB) RootCounts(rootID int64) (Counts, error) {
	return d.counts(`WHERE root_id=?`, rootID)
}

func (d *DB) VolumeCounts(volID string) (Counts, error) {
	return d.counts(`WHERE volume_id=?`, volID)
}

func (d *DB) AllCounts() (Counts, error) { return d.counts(`WHERE ?`, 1) }

// RootStat 是一个索引根的统计：各状态的文件数，以及待处理里老格式、OCR 各有多少。
type RootStat struct {
	Counts
	LegacyPending int64
	OCRPending    int64
}

// StatsByRoot 一次查询（只读覆盖索引）得到全部索引根的统计，代替逐个索引根、逐个卷的多次全表扫描。
func (d *DB) StatsByRoot(prioLegacy, prioOCR int) (map[int64]*RootStat, error) {
	rows, err := d.R.Query(`SELECT root_id, state, prio, COUNT(*), COALESCE(SUM(ocr),0) FROM files GROUP BY root_id, state, prio`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*RootStat{}
	for rows.Next() {
		var root, n, ocr int64
		var state, prio int
		if err := rows.Scan(&root, &state, &prio, &n, &ocr); err != nil {
			return nil, err
		}
		st := out[root]
		if st == nil {
			st = &RootStat{}
			out[root] = st
		}
		st.Total += n
		st.OCR += ocr
		switch state {
		case StatePending:
			st.Pending += n
			switch prio {
			case prioLegacy:
				st.LegacyPending += n
			case prioOCR:
				st.OCRPending += n
			}
		case StateDone:
			st.Done += n
		case StateFailed:
			st.Failed += n
		case StateEncrypted:
			st.Encrypted += n
		case StateMetaOnly:
			st.MetaOnly += n
		}
	}
	return out, rows.Err()
}

func (d *DB) counts(where string, arg any) (Counts, error) {
	var c Counts
	err := d.R.QueryRow(`SELECT COUNT(*),
COALESCE(SUM(state=0),0),COALESCE(SUM(state=1),0),COALESCE(SUM(state=2),0),COALESCE(SUM(state=3),0),COALESCE(SUM(state=4),0),COALESCE(SUM(ocr),0)
FROM files `+where, arg).Scan(&c.Total, &c.Pending, &c.Done, &c.Failed, &c.Encrypted, &c.MetaOnly, &c.OCR)
	return c, err
}

// ListFailed 返回失败与加密的文件。
func (d *DB) ListFailed(limit int) ([]File, error) {
	rows, err := d.R.Query(`SELECT `+fileCols+` FROM files WHERE state IN (2,3) ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RetryFailed 把指定的失败文件重新置为待提取；ids 为空时重试所有失败文件。
// MoveLegacy 把待处理的老格式文件（exts）移到 prio 队列。retryFailed 时把以前提取失败的也重新排队。
// 只处理磁盘上的文件本身，压缩包里的文件随压缩包一起提取。
func (d *DB) MoveLegacy(exts []string, prio int, retryFailed bool) (moved, retried int64, err error) {
	if len(exts) == 0 {
		return 0, 0, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(exts)), ",")
	args := []any{prio}
	for _, e := range exts {
		args = append(args, e)
	}
	err = d.Tx(func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE files SET prio=? WHERE state=0 AND inner='' AND prio IN (1,2) AND ext IN (`+in+`)`, args...)
		if err != nil {
			return err
		}
		moved, _ = r.RowsAffected()
		if retryFailed {
			r, err := tx.Exec(`UPDATE files SET state=0, err='', prio=? WHERE state=2 AND inner='' AND ext IN (`+in+`)`, args...)
			if err != nil {
				return err
			}
			retried, _ = r.RowsAffected()
		}
		return nil
	})
	return moved, retried, err
}

// RequeueEncrypted 把标记为「已加密」的这些扩展名的文件重新排队（新版本能读出其中一部分时用）。
func (d *DB) RequeueEncrypted(exts []string, prio int) (int64, error) {
	if len(exts) == 0 {
		return 0, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(exts)), ",")
	args := []any{prio}
	for _, e := range exts {
		args = append(args, e)
	}
	r, err := d.W.Exec(`UPDATE files SET state=0, err='', prio=? WHERE state=3 AND inner='' AND ext IN (`+in+`)`, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// PDFInfo 是复查 PDF 时用到的文件信息。
type PDFInfo struct {
	ID, Size int64
	TextLen  int
}

// DonePDFs 按 id 顺序返回已提取、没有用过 OCR 的 PDF（磁盘上的文件本身）。
func (d *DB) DonePDFs(after int64, limit int) ([]PDFInfo, error) {
	rows, err := d.R.Query(`SELECT id, size, text_len FROM files WHERE ext='pdf' AND state=1 AND ocr=0 AND inner='' AND id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PDFInfo
	for rows.Next() {
		var p PDFInfo
		if err := rows.Scan(&p.ID, &p.Size, &p.TextLen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Requeue 把文件重新排队提取。已有的文字在重新提取完成前照样能搜到。
func (d *DB) Requeue(ids []int64, prio int) error {
	return d.Tx(func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE files SET state=0, prio=? WHERE id=?`, prio, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *DB) RetryFailed(ids []int64) error {
	return d.Tx(func(tx *sql.Tx) error {
		if len(ids) == 0 {
			_, err := tx.Exec(`UPDATE files SET state=0, err='' WHERE state=2`)
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE files SET state=0, err='' WHERE id=? AND state IN (2,3)`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResetAllContent 把所有文件重置为待提取（重建索引）。
func (d *DB) ResetContent(rootID int64) error {
	return d.Tx(func(tx *sql.Tx) error {
		cond, arg := `1=?`, any(1)
		if rootID != 0 {
			cond, arg = `root_id=?`, rootID
		}
		// 旧文本保留到重新提取完成时替换，期间仍可搜索；容器内部条目随容器重新提取而重建。
		for _, q := range []string{
			`UPDATE files SET state=0, err='', indexed_at=0 WHERE ` + cond + ` AND inner=''`,
		} {
			if _, err := tx.Exec(q, arg); err != nil {
				return err
			}
		}
		return deleteInner(tx, cond, arg)
	})
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func deleteInner(tx *sql.Tx, cond string, arg any) error {
	for _, q := range []string{
		`DELETE FROM fts WHERE rowid IN (SELECT id FROM files WHERE ` + cond + ` AND inner<>'')`,
		`DELETE FROM files WHERE ` + cond + ` AND inner<>''`,
	} {
		if _, err := tx.Exec(q, arg); err != nil {
			return err
		}
	}
	return nil
}

// RebuildFTS 清空全文索引，由调用方用 FTSRows 的结果重新写入（比如归一化设置改变后）。
func (d *DB) ClearFTS() error {
	return d.Tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM fts`)
		return err
	})
}

// Ranked 是带相关度的候选文件（bm25 值越小越相关）。
type Ranked struct {
	File
	Rank float64
}

func prefixCols(prefix string) string {
	cols := strings.Split(fileCols, ",")
	for i := range cols {
		cols[i] = prefix + cols[i]
	}
	return strings.Join(cols, ",")
}

// QueryRanked 用 FTS5 表达式取候选文件；match 为空时返回全部文件。
// 文件名列权重高于内容列。
func (d *DB) QueryRanked(match string) ([]Ranked, error) {
	var rows *sql.Rows
	var err error
	if match == "" {
		rows, err = d.R.Query(`SELECT ` + prefixCols("f.") + `, 0 FROM files f`)
	} else {
		rows, err = d.R.Query(`SELECT `+prefixCols("f.")+`, bm25(fts, 1.0, 8.0) FROM fts JOIN files f ON f.id = fts.rowid WHERE fts MATCH ?`, match)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ranked
	for rows.Next() {
		var f File
		var ocr int
		var rank float64
		if err := rows.Scan(&f.ID, &f.RootID, &f.VolumeID, &f.RelPath, &f.Inner, &f.Name, &f.Ext, &f.Kind, &f.Size, &f.MTime,
			&f.State, &f.Err, &f.Hash, &f.TextLen, &ocr, &f.Meta, &f.Prio, &rank); err != nil {
			return nil, err
		}
		f.OCR = ocr != 0
		out = append(out, Ranked{File: f, Rank: rank})
	}
	return out, rows.Err()
}

// GetSig 取一个顶层文件的签名。
func (d *DB) GetSig(volumeID, rel string) (FileSig, bool) {
	var s FileSig
	err := d.R.QueryRow(`SELECT id,size,mtime,state FROM files WHERE volume_id=? AND rel_path=? AND inner=''`, volumeID, rel).
		Scan(&s.ID, &s.Size, &s.MTime, &s.State)
	return s, err == nil
}

// VolumeIndexBytes 估算某个卷的索引占用空间（压缩后的提取文本大小）。
func (d *DB) VolumeIndexBytes(volID string) (int64, error) {
	var n sql.NullInt64
	err := d.R.QueryRow(`SELECT SUM(length(data)) FROM texts WHERE id IN (SELECT text_id FROM files WHERE volume_id=? AND text_id>0)`, volID).Scan(&n)
	return n.Int64, err
}

// CountMatch 返回 FTS5 表达式命中的文件数（只扫描倒排索引，很快）。
func (d *DB) CountMatch(match string) (int, error) {
	var n int
	err := d.R.QueryRow(`SELECT count(*) FROM fts WHERE fts MATCH ?`, match).Scan(&n)
	return n, err
}

// MatchIDs 返回 FTS5 表达式命中的全部文件 id。
func (d *DB) MatchIDs(match string) (map[int64]bool, error) {
	rows, err := d.R.Query(`SELECT rowid FROM fts WHERE fts MATCH ?`, match)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// TopRanked 取相关度最高的 k 个候选。大量命中时只为前 k 个构造文件对象，避免整体开销。
func (d *DB) TopRanked(match string, k int) ([]Ranked, error) {
	rows, err := d.R.Query(`SELECT rowid, bm25(fts, 1.0, 8.0) r FROM fts WHERE fts MATCH ? ORDER BY r LIMIT ?`, match, k)
	if err != nil {
		return nil, err
	}
	var ids []int64
	rank := map[int64]float64{}
	for rows.Next() {
		var id int64
		var r float64
		if err := rows.Scan(&id, &r); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		rank[id] = r
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	byID := make(map[int64]File, len(ids))
	for i := 0; i < len(ids); i += 500 {
		j := i + 500
		if j > len(ids) {
			j = len(ids)
		}
		chunk := ids[i:j]
		args := make([]any, len(chunk))
		for n, id := range chunk {
			args[n] = id
		}
		fr, err := d.R.Query(`SELECT `+fileCols+` FROM files WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for fr.Next() {
			f, err := scanFile(fr)
			if err != nil {
				fr.Close()
				return nil, err
			}
			byID[f.ID] = f
		}
		fr.Close()
	}
	out := make([]Ranked, 0, len(ids))
	for _, id := range ids {
		if f, ok := byID[id]; ok {
			out = append(out, Ranked{File: f, Rank: rank[id]})
		}
	}
	return out, nil
}

// upgradeSchema 把旧版本的索引库升级到当前格式。升级只改表结构（很快），
// 旧数据的重新压缩、合并重复文本在后台逐步进行（F-BG-13）。
func upgradeSchema(w *sql.DB) error {
	var has int
	if err := w.QueryRow(`SELECT count(*) FROM pragma_table_info('files') WHERE name='text_id'`).Scan(&has); err != nil {
		return err
	}
	if has > 0 {
		return nil
	}
	// 0.1.x：texts 以 file_id 为主键，一个文件一份。file_id 原样作为文本编号，文件的 text_id 指向它。
	tx, err := w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`ALTER TABLE files ADD COLUMN text_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE texts RENAME COLUMN file_id TO id`,
		`UPDATE files SET text_id=id WHERE id IN (SELECT id FROM texts)`,
		`DELETE FROM texts WHERE id NOT IN (SELECT text_id FROM files)`, // 旧版本可能留下的孤立文本
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return tx.Commit()
}
