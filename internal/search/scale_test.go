package search

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/norm"
	"github.com/chenonestar/zhaozhao/internal/store"
)

// 规模测试：ZHAOZHAO_SCALE=100000 go test ./internal/search -run TestScale -v -timeout 30m
// 生成 N 篇合成文档直接写入索引库，实测各类查询的延迟（需求第 9 节性能目标）。
func TestScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ZHAOZHAO_SCALE"))
	if n == 0 {
		t.Skip("未设置 ZHAOZHAO_SCALE")
	}
	rng := rand.New(rand.NewSource(1))
	// 词表：常用汉字构成的随机「词」和英文词
	han := []rune("的一是不了人我在有他这中大来上国个到说们为子和你地出道也时年得就那要下以生会自着去之过家学对可她里后小么心多天而能好都然没日于起还发成事只作当想看文无开手十用主行方又如前所本见经头面公同三已老从动两长知民样现分将外但身些与高意进把法此实回二理美点月明其种声全工话儿者向情部正名定女问力机给等几很业最间新什少它重")
	words := []string{"report", "invoice", "project", "meeting", "budget", "contract", "schedule", "review", "summary", "status", "quarterly", "delivery", "customer", "payment", "approval"}
	word := func() string {
		if rng.Intn(4) == 0 {
			return words[rng.Intn(len(words))]
		}
		l := 2 + rng.Intn(3)
		rs := make([]rune, l)
		for i := range rs {
			// 偏斜分布：前面的字更常用
			rs[i] = han[int(float64(len(han))*rng.Float64()*rng.Float64())]
		}
		return string(rs)
	}
	dbDir := t.TempDir()
	if d := os.Getenv("ZHAOZHAO_SCALE_KEEP"); d != "" {
		dbDir = d
	}
	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o := norm.Options{Simplify: true}
	rid, _ := db.AddRoot("vol1", "")
	t0 := time.Now()
	const batch = 500
	perDoc := 400 // 平均词数 → 约 3KB 文本
	for start := 0; start < n; start += batch {
		err := db.Tx(func(tx *sql.Tx) error {
			for i := start; i < start+batch && i < n; i++ {
				var sb strings.Builder
				for w := 0; w < perDoc/2+rng.Intn(perDoc); w++ {
					sb.WriteString(word())
					if rng.Intn(9) == 0 {
						sb.WriteString("。\n")
					} else {
						sb.WriteByte(' ')
					}
				}
				if i%1000 == 7 {
					sb.WriteString("联系电话 13812345678 与 张三 报销 ")
				}
				if i%5000 == 3 {
					sb.WriteString(" 不可抗力条款 ")
				}
				text := sb.String()
				name := fmt.Sprintf("文件%06d-%s.txt", i, words[i%len(words)])
				f := &store.File{RootID: rid, VolumeID: "vol1", RelPath: fmt.Sprintf("d%d/%s", i%500, name), Name: name, Ext: "txt",
					Kind: extract.KindText, Size: int64(len(text)), MTime: time.Now().Unix() - int64(i), State: store.StatePending}
				nf := norm.FTSText(norm.Tokenize(o.Runes(name)))
				if err := store.UpsertFile(tx, f, nf); err != nil {
					return err
				}
				fts := norm.FTSText(norm.Tokenize(o.Runes(text)))
				if err := store.PutResult(tx, f.ID, store.StateDone, "", "", false, &store.Text{Text: text, LineBased: true}, fts, nf); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("写入 %d 篇文档用时 %v", n, time.Since(t0).Round(time.Second))
	var sz int64
	db.R.QueryRow(`SELECT page_count*page_size FROM pragma_page_count, pragma_page_size`).Scan(&sz)
	var raw int64
	db.R.QueryRow(`SELECT SUM(text_len) FROM files`).Scan(&raw)
	t.Logf("索引库 %.0f MB，提取文本共 %.0f M 字符（索引 / 文本字符数 = %.2f）", float64(sz)/1e6, float64(raw)/1e6, float64(sz)/float64(raw*3))

	e := &Engine{DB: db, Res: stubRes{}, Norm: func() norm.Options { return o }}
	run := func(label, q string, p Params) {
		p.Query = q
		best := time.Hour
		var resp *Response
		for i := 0; i < 3; i++ {
			s := time.Now()
			var err error
			resp, err = e.Search(context.Background(), p)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if d := time.Since(s); d < best {
				best = d
			}
		}
		t.Logf("%-28s %-30s 命中 %-7d exact=%-5v %v", label, q, resp.Total, resp.Exact, best.Round(time.Millisecond))
	}
	run("单字（高频）", "的", Params{})
	run("两字词", "报销", Params{})
	run("两字词 AND", "张三 报销", Params{})
	run("短语", `"不可抗力条款"`, Params{})
	run("布尔 OR/NOT", "报销 OR 不可抗力 -联系", Params{})
	run("英文前缀", "quart", Params{})
	run("英文 AND 中文", "report 张三", Params{})
	run("仅文件名", "文件000123", Params{})
	run("过滤 ext+size", "ext:txt size:>1KB 张三", Params{})
	run("NEAR", "张三 NEAR/5 报销", Params{})
	run("模糊(中文)", "不可抗条款~", Params{})
	run("模糊(英文)", "quarterlt~", Params{})
	run("正则(有汉字字面量)", `/张三.*报销/`, Params{})
	run("正则(手机号, 全量扫描)", `/1[3-9]\d{9}/`, Params{})
}
