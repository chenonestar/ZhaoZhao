package store

import (
	"bytes"
	"context"
	"errors"
)

// 全文索引（FTS5）由若干「段」组成：每次写入生成一个小段，同一层攒够 4 个段就合并成上一层的一个大段。
// 段多了，同一个词在每个段里各记一份词典，已删除、已重新提取的文件在旧段里的条目也要等合并时才清掉；
// 全部合并成一个段，索引能小一成左右。FTS5 的 optimize 命令一次合并完，大索引库要几分钟，期间不能写入；
// 这里用 merge 命令分小步做（MergeFTS），每步只写几 MB，随时可以停下，下次接着做。

// FTSInfo 是全文索引的段结构概况（读一条记录，很快）。
type FTSInfo struct {
	Segments int   `json:"segments"` // 段数
	Pages    int64 `json:"pages"`    // 各段的页数之和（每页约 4 KB）
	Largest  int64 `json:"largest"`  // 最大的段的页数
	Entries  int64 `json:"entries"`  // 各段里的文件条目数之和
	Deleted  int64 `json:"deleted"`  // 已删除、等合并时清掉的条目数
	Merging  int64 `json:"merging"`  // 正在合并的段还剩多少页（0 表示没有在合并）
}

var errFTSStructure = errors.New("全文索引结构无法解析")

// FTSInfo 读出全文索引的段结构。
func (d *DB) FTSInfo() (FTSInfo, error) {
	var b []byte
	// 结构记录存在 fts_data 的第 10 行（FTS5_STRUCTURE_ROWID）
	if err := d.R.QueryRow(`SELECT block FROM fts_data WHERE id=10`).Scan(&b); err != nil {
		return FTSInfo{}, err
	}
	return parseFTSStructure(b)
}

// parseFTSStructure 解析 FTS5 的结构记录（格式见 SQLite 源码 fts5_index.c 的 fts5StructureDecode）：
// 4 字节 cookie，[V2 标记]，层数，段数，写入计数，然后每层：正在合并的段数、段数，每段：段号、首页、末页，
// [V2：两个来源计数、墓碑页数、墓碑条目数、条目数]。
func parseFTSStructure(b []byte) (FTSInfo, error) {
	var info FTSInfo
	if len(b) < 4 {
		return info, errFTSStructure
	}
	i := 4
	v2 := len(b) >= 8 && bytes.Equal(b[4:8], []byte{0xff, 0, 0, 1})
	if v2 {
		i = 8
	}
	bad := false
	next := func() int64 {
		v, n := sqliteVarint(b[min(i, len(b)):])
		if n == 0 {
			bad = true
		}
		i += n
		return int64(v)
	}
	levels, segs := next(), next()
	next() // 写入计数
	if bad || levels < 0 || segs < 0 || levels > 64 || segs > 2000 {
		return info, errFTSStructure
	}
	for l := int64(0); l < levels && !bad; l++ {
		merging, n := next(), next()
		if n < merging || n > segs {
			return info, errFTSStructure
		}
		for s := int64(0); s < n && !bad; s++ {
			next() // 段号
			first, last := next(), next()
			if v2 {
				next()
				next()
				next()
				info.Deleted += next()
				info.Entries += next()
			}
			if last < first {
				return info, errFTSStructure
			}
			pages := last - first + 1
			info.Segments++
			info.Pages += pages
			info.Largest = max(info.Largest, pages)
			if s < merging {
				info.Merging += pages
			}
		}
	}
	if bad || int64(info.Segments) != segs {
		return info, errFTSStructure
	}
	return info, nil
}

// sqliteVarint 读 SQLite 格式的变长整数（高位在前，每字节 7 位，第 9 个字节用满 8 位）；n 为 0 表示数据不完整。
func sqliteVarint(b []byte) (v uint64, n int) {
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return v<<8 | uint64(b[i]), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// MergeFTS 合并全文索引的段，最多写入约 pages 页。start 为 true 时开始一次完整合并（把现有的段都合成一个），
// 否则继续进行中的合并。more 为 false 表示这一步没有可合并的了（完整合并已经做完）。
func (d *DB) MergeFTS(pages int, start bool) (more bool, err error) {
	ctx := context.Background()
	c, err := d.W.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close()
	n := pages
	if start {
		n = -pages // 负数：先把所有段放到同一层再合并（相当于分步的 optimize）
	}
	var before, after int64
	if err := c.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		return false, err
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO fts(fts, rank) VALUES('merge', ?)`, n); err != nil {
		return false, err
	}
	if err := c.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
		return false, err
	}
	// SQLite 文档：前后相差不到 2 表示 merge 什么也没做
	return after-before >= 2, nil
}
