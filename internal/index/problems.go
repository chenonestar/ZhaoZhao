package index

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenonestar/zhaozhao/internal/store"
)

// RetryProblems 把失败列表里符合条件的失败、已加密文件重新排队，返回重新排队的数量。
func (m *Manager) RetryProblems(f store.ProblemFilter) (int64, error) {
	n, err := m.DB.RetryProblems(f)
	if err != nil {
		return 0, err
	}
	m.kick()
	return n, nil
}

// problemScope 返回筛选范围的名称（用作导出文件名）。
func problemScope(f store.ProblemFilter) string {
	s := "失败和已加密"
	switch f.Cat {
	case "":
	case "meta":
		s = "仅文件名"
	default:
		s = store.ProblemLabel(f.Cat)
	}
	if f.Ext != "" {
		e := f.Ext
		if e == "." {
			e = "无扩展名"
		}
		s += "_" + e
	}
	return strings.NewReplacer("/", "、", `\`, "、", ":", "", "*", "", "?", "", `"`, "", "<", "", ">", "", "|", "").Replace(s)
}

var stateNames = map[int]string{store.StateFailed: "失败", store.StateEncrypted: "已加密", store.StateMetaOnly: "仅文件名"}

// ExportProblems 把失败列表里符合条件的文件导出为 CSV（UTF-8 带 BOM，Excel 可以直接打开），
// 放在数据目录的「导出」文件夹里，返回文件路径和导出的行数。
func (m *Manager) ExportProblems(f store.ProblemFilter) (string, int, error) {
	dir := filepath.Join(m.DataDir, "导出")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	p := filepath.Join(dir, fmt.Sprintf("失败列表_%s_%s.csv", problemScope(f), time.Now().Format("20060102-150405")))
	out, err := os.Create(p)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	out.WriteString("\xef\xbb\xbf") // BOM：Excel 按 UTF-8 打开
	w := csv.NewWriter(out)
	w.UseCRLF = true
	_ = w.Write([]string{"路径", "状态", "原因类别", "原因", "扩展名", "大小（字节）", "修改时间"})
	n := 0
	err = m.DB.EachProblem(f, func(x *store.Problem) error {
		path, _, _ := m.Display(&x.File)
		mt := ""
		if x.MTime > 0 {
			mt = time.Unix(x.MTime, 0).Format("2006-01-02 15:04:05")
		}
		n++
		return w.Write([]string{path, stateNames[x.State], store.ProblemLabel(x.Cat), x.Err, x.Ext, fmt.Sprint(x.Size), mt})
	})
	if err == nil {
		w.Flush()
		err = w.Error()
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		os.Remove(p)
		return "", 0, err
	}
	return p, n, nil
}
