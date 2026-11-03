package index

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
	"github.com/chenonestar/zhaozhao/internal/worker"
)

// 0.2.12 及以前识别的扫描 PDF，大幅面页（比 A4 大：图纸、A3 表格）也缩小了找文字，小字常常没认出来（0.2.12
// 某位用户的 OCR 引擎对比：一个 A3 页平常认出 963 字，大图 1570 字）。0.2.13 起大幅面页用大图找字；旧文件用
// 「检查大幅面页」找出来重新识别：逐个打开这些 PDF（不识别，只看需要 OCR 的页有多大，每个文件几十毫秒到一两秒），
// 有大幅面页的重新排到 OCR 队列，没有的记下查过了。

// BigPageState 是检查大幅面页的进度。
type BigPageState struct {
	Running    bool   `json:"running"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Found      int    `json:"found"`      // 有大幅面页、已重新排队的文件数
	Pages      int    `json:"pages"`      // 其中的大幅面页数
	Candidates int64  `json:"candidates"` // 还要查的文件数（0.2.12 及以前 OCR 识别的 PDF）
	Err        string `json:"err,omitempty"`
}

type bigPageChecker struct {
	mu     sync.Mutex
	state  BigPageState
	cancel context.CancelFunc
}

// BigPages 返回检查大幅面页的进度和还要查的文件数。
func (m *Manager) BigPages() BigPageState {
	t := &m.bigChk
	t.mu.Lock()
	st := t.state
	t.mu.Unlock()
	st.Candidates, _ = m.DB.CountBigPageCandidates(prioOCR)
	return st
}

// CancelBigPages 停止检查（已查过的不会再查）。
func (m *Manager) CancelBigPages() {
	t := &m.bigChk
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

// StartBigPages 在后台检查 0.2.12 及以前识别的 PDF 有没有大幅面页，有的重新排到 OCR 队列。
func (m *Manager) StartBigPages() error {
	if !m.ocrAvailable() || !m.Config().OCREnabled {
		return fmt.Errorf("OCR 组件没有安装或 OCR 已关闭")
	}
	total, err := m.DB.CountBigPageCandidates(prioOCR)
	if err != nil {
		return err
	}
	if total == 0 {
		return fmt.Errorf("没有要查的文件：0.2.12 及以前识别的扫描 PDF 都查过了")
	}
	t := &m.bigChk
	t.mu.Lock()
	if t.state.Running {
		t.mu.Unlock()
		return fmt.Errorf("正在检查")
	}
	ctx, cancel := context.WithCancel(m.ctx)
	t.cancel = cancel
	t.state = BigPageState{Running: true, Total: int(total)}
	t.mu.Unlock()
	m.logf("开始检查大幅面页：0.2.12 及以前识别的 %d 个扫描 PDF", total)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		err := m.runBigPages(ctx)
		t.mu.Lock()
		t.state.Running, t.cancel = false, nil
		if err != nil && ctx.Err() == nil {
			t.state.Err = err.Error()
		}
		st := t.state
		t.mu.Unlock()
		m.logf("检查大幅面页%s：查了 %d 个文件，%d 个有大幅面页（共 %d 页），已重新排到 OCR 队列",
			map[bool]string{true: "（中途停止）", false: "完成"}[ctx.Err() != nil], st.Done, st.Found, st.Pages)
		if st.Found > 0 {
			m.kick()
		}
	}()
	return nil
}

// bigPageRunner 查一个 PDF 的大幅面页（测试里换成模拟的）。
var bigPageRunner = func(m *Manager, ctx context.Context, path string) doc.Result {
	o := extract.DefaultOptions()
	o.BigPageCheck = true
	return m.Pool.Run(ctx, path, "pdf", o, worker.Timeouts{Total: 2 * time.Minute}, nil)
}

func (m *Manager) runBigPages(ctx context.Context) error {
	offline := map[string]bool{}
	for _, v := range m.offlineVolumes() {
		offline[v] = true
	}
	var after int64
	for ctx.Err() == nil {
		fs, err := m.DB.BigPageCandidates(prioOCR, after, 200)
		if err != nil {
			return err
		}
		if len(fs) == 0 {
			return nil
		}
		for _, f := range fs {
			if ctx.Err() != nil {
				return nil
			}
			after = f.ID
			path, _ := m.AbsPath(f.VolumeID, f.RelPath)
			if offline[f.VolumeID] || path == "" {
				continue // 所在的盘没接上：下次再查
			}
			res := bigPageRunner(m, ctx, path)
			if ctx.Err() != nil {
				return nil
			}
			switch {
			case res.Status != doc.StatusOK:
				logx.Debugf("检查大幅面页 %s：%s", logx.P(path), res.Err) // 打不开的下次再查
			case res.BigPages > 0:
				if err := m.DB.RequeueOCR(prioOCR, f.ID); err != nil {
					return err
				}
				m.bigChk.mu.Lock()
				m.bigChk.state.Found++
				m.bigChk.state.Pages += res.BigPages
				m.bigChk.mu.Unlock()
				logx.Debugf("%s 有 %d 页大幅面页，重新识别", logx.P(path), res.BigPages)
			default:
				if err := m.DB.SetBigChecked(f.ID); err != nil {
					return err
				}
			}
			m.bigChk.mu.Lock()
			m.bigChk.state.Done++
			m.bigChk.mu.Unlock()
		}
	}
	return nil
}
