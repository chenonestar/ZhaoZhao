package index

import (
	"github.com/chenonestar/zhaozhao/internal/extract"
)

// CheckComponents 在后台检测 LibreOffice 和 OCR 组件能否真正工作，结果显示在「索引」页。
// 组件不能用时，老格式和图片会大量失败或长时间卡住，用户需要第一时间知道原因。
func (m *Manager) CheckComponents() {
	m.compMu.Lock()
	if m.compChecking {
		m.compMu.Unlock()
		return
	}
	m.compChecking = true
	m.compMu.Unlock()
	ocr := m.ocrCfg
	go func() {
		res := []extract.ComponentStatus{extract.CheckSoffice(), extract.CheckOCR(ocr)}
		for _, c := range res {
			m.logf("组件自检：%s：%s %s", c.Name, c.Message, c.Path)
		}
		m.compMu.Lock()
		m.components, m.compChecking = res, false
		m.compMu.Unlock()
	}()
}

// Components 返回最近一次自检结果。
func (m *Manager) Components() ([]extract.ComponentStatus, bool) {
	m.compMu.Lock()
	defer m.compMu.Unlock()
	out := append([]extract.ComponentStatus{}, m.components...)
	return out, m.compChecking
}
