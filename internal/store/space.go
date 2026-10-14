package store

import (
	"database/sql"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
)

// SpaceInfo 是索引库文件的空间概况（很快，只读几个 PRAGMA）。
type SpaceInfo struct {
	DBBytes     int64 `json:"dbBytes"`     // index.db 文件大小
	WALBytes    int64 `json:"walBytes"`    // index.db-wal 文件大小
	FreeBytes   int64 `json:"freeBytes"`   // 空闲页（删除内容后空出来、还没还给磁盘的空间）
	Incremental bool  `json:"incremental"` // 空闲页会在空闲时逐步还给磁盘
}

// Space 返回索引库文件的空间概况。
func (d *DB) Space() SpaceInfo {
	var s SpaceInfo
	if st, err := os.Stat(d.Path); err == nil {
		s.DBBytes = st.Size()
	}
	if st, err := os.Stat(d.Path + "-wal"); err == nil {
		s.WALBytes = st.Size()
	}
	var free, pageSize, av int64
	_ = d.R.QueryRow(`PRAGMA freelist_count`).Scan(&free)
	_ = d.R.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	_ = d.R.QueryRow(`PRAGMA auto_vacuum`).Scan(&av)
	s.FreeBytes = free * pageSize
	s.Incremental = av == 2
	return s
}

// Checkpoint 把 WAL 里的内容写回主文件并把 WAL 截短为 0（空闲时调用）。
func (d *DB) Checkpoint() error {
	_, err := d.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// IncrementalVacuum 把最多 pages 个空闲页还给磁盘（只对增量回收模式的索引库有效）。
func (d *DB) IncrementalVacuum(pages int) error {
	_, err := d.W.Exec(`PRAGMA incremental_vacuum(?)`, pages)
	return err
}

// Vacuum 重写整个索引库，把空闲空间全部还给磁盘，并改为增量回收模式（以后删除内容空出的空间
// 会在空闲时逐步归还，不必再手动压缩）。需要与索引库差不多大的临时空间，期间写入会等待。
func (d *DB) Vacuum() error {
	if _, err := d.W.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return err
	}
	if _, err := d.W.Exec(`VACUUM`); err != nil {
		return err
	}
	return d.Checkpoint()
}

// SpaceItem 是空间统计里的一项。
type SpaceItem struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Count int64  `json:"count,omitempty"`
}

// BigText 是占用空间最多的提取文本之一。
type BigText struct {
	FileID  int64 `json:"fileId"`
	Bytes   int64 `json:"bytes"`   // 压缩后
	Chars   int64 `json:"chars"`   // 文字数
	Sharers int64 `json:"sharers"` // 共用这份文本的文件数
}

// SpaceStats 是索引库空间的详细统计（要读遍整个索引库，大索引库需要几秒到几十秒）。
type SpaceStats struct {
	SpaceInfo
	Parts     []SpaceItem `json:"parts"`     // 全文索引、提取文本、文件信息……各占多少
	ByExt     []SpaceItem `json:"byExt"`     // 各扩展名的提取文本（压缩后）大小与文件数，前 20
	Biggest   []BigText   `json:"biggest"`   // 最大的 20 份提取文本
	Texts     int64       `json:"texts"`     // 文本份数
	TextFiles int64       `json:"textFiles"` // 有文本的文件数（多于份数的部分是共用的）
	RawChars  int64       `json:"rawChars"`  // 全部文本的文字数
	Legacy    int64       `json:"legacy"`    // 还是旧格式（未重新压缩）的文本份数
}

// partName 把 SQLite 内部对象名翻译成能看懂的类别。
func partName(n string) string {
	switch {
	case strings.HasPrefix(n, "fts"):
		return "全文索引"
	case n == "texts":
		return "提取文本"
	case n == "files" || strings.HasPrefix(n, "files_") || strings.HasPrefix(n, "sqlite_autoindex_files"):
		return "文件信息"
	}
	return "其他"
}

// Stats 统计索引库空间。
func (d *DB) Stats() (*SpaceStats, error) {
	st := &SpaceStats{SpaceInfo: d.Space()}
	rows, err := d.R.Query(`SELECT name, sum(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		return nil, err
	}
	parts := map[string]int64{}
	for rows.Next() {
		var n string
		var b int64
		if err := rows.Scan(&n, &b); err != nil {
			rows.Close()
			return nil, err
		}
		parts[partName(n)] += b
	}
	rows.Close()
	for _, n := range []string{"全文索引", "提取文本", "文件信息", "其他"} {
		if parts[n] > 0 {
			st.Parts = append(st.Parts, SpaceItem{Name: n, Bytes: parts[n]})
		}
	}
	if st.FreeBytes > 0 {
		st.Parts = append(st.Parts, SpaceItem{Name: "空闲（可回收）", Bytes: st.FreeBytes})
	}
	_ = d.R.QueryRow(`SELECT count(*), ifnull(sum((flags & 2)=0),0) FROM texts`).Scan(&st.Texts, &st.Legacy)
	_ = d.R.QueryRow(`SELECT count(*), ifnull(sum(text_len),0) FROM files WHERE text_id>0`).Scan(&st.TextFiles, &st.RawChars)

	// 各扩展名：共用的文本只算给其中一个文件
	rows, err = d.R.Query(`SELECT f.ext, count(*), sum(length(t.data)+ifnull(length(t.locs),0)) FROM texts t
JOIN files f ON f.id=(SELECT min(id) FROM files WHERE text_id=t.id) GROUP BY f.ext`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it SpaceItem
		if err := rows.Scan(&it.Name, &it.Count, &it.Bytes); err != nil {
			rows.Close()
			return nil, err
		}
		if it.Name == "" {
			it.Name = "（无扩展名）"
		}
		st.ByExt = append(st.ByExt, it)
	}
	rows.Close()
	sort.Slice(st.ByExt, func(i, j int) bool { return st.ByExt[i].Bytes > st.ByExt[j].Bytes })
	if len(st.ByExt) > 20 {
		st.ByExt = st.ByExt[:20]
	}

	rows, err = d.R.Query(`SELECT t.id, length(t.data)+ifnull(length(t.locs),0) b FROM texts t ORDER BY b DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	var big []BigText
	var ids []int64
	for rows.Next() {
		var id int64
		var bt BigText
		if err := rows.Scan(&id, &bt.Bytes); err != nil {
			rows.Close()
			return nil, err
		}
		big = append(big, bt)
		ids = append(ids, id)
	}
	rows.Close()
	for i, id := range ids {
		_ = d.R.QueryRow(`SELECT min(id), count(*), max(text_len) FROM files WHERE text_id=?`, id).Scan(&big[i].FileID, &big[i].Sharers, &big[i].Chars)
	}
	st.Biggest = big
	return st, nil
}

// randomTextIDs 随机取最多 n 个文本编号（按编号范围随机探测，不用 ORDER BY random() 扫全表）。
func (d *DB) randomTextIDs(n int) ([]int64, error) {
	var maxID sql.NullInt64
	if err := d.R.QueryRow(`SELECT max(id) FROM texts`).Scan(&maxID); err != nil || !maxID.Valid {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []int64
	for tries := 0; tries < n*3 && len(out) < n; tries++ {
		var id int64
		if err := d.R.QueryRow(`SELECT id FROM texts WHERE id>=? ORDER BY id LIMIT 1`, 1+rand.Int64N(maxID.Int64)).Scan(&id); err != nil {
			continue
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}
