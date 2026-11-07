package index

import (
	"github.com/chenonestar/zhaozhao/internal/extract"
	"github.com/chenonestar/zhaozhao/internal/logx"
)

// CheckComponents 在后台检测 LibreOffice、OCR 和 PDFium 组件能否真正工作，结果显示在「索引」页。
// 组件不能用时，老格式和图片会大量失败或长时间卡住，用户需要第一时间知道原因。
func (m *Manager) CheckComponents() {
	m.compMu.Lock()
	if m.compChecking {
		m.compMu.Unlock()
		return
	}
	m.compChecking = true
	m.compMu.Unlock()
	model := m.ocrModel()
	go func() {
		res := []extract.ComponentStatus{extract.CheckSoffice(), m.checkOCR(model), extract.CheckPDFium()}
		for i, c := range res {
			if i != 1 {
				m.logf("组件自检：%s：%s %s", c.Name, c.Message, logx.P(c.Path))
			}
		}
		m.compMu.Lock()
		m.components, m.compModel, m.compChecking = res, model, false
		m.compMu.Unlock()
		m.recheckOCRModel() // 自检期间认字模型变了（测速完成、改了设置）
	}()
}

// checkOCR 用认字模型 model 检查 OCR 组件能否工作，并写进日志。
func (m *Manager) checkOCR(model string) extract.ComponentStatus {
	ocr := m.ocrCfg.WithModel(model)
	st := extract.CheckOCR(ocr)
	if ocr.Enabled() {
		st.Message += "，认字模型 " + extract.OCRModelName(model)
		if m.Config().OCRModel == "" {
			st.Message += "（自动）"
		}
	}
	m.logf("组件自检：%s：%s %s", st.Name, st.Message, logx.P(st.Path))
	return st
}

// recheckOCRModel：认字模型变了（改了设置、测速后）时重新检查 OCR，自检结果写明实际用的模型。
// 0.2.7 的自检在测速前做，写的是还没测速时的模型。
func (m *Manager) recheckOCRModel() {
	model := m.ocrModel()
	m.compMu.Lock()
	if m.compChecking || len(m.components) < 2 || m.compModel == model {
		m.compMu.Unlock()
		return
	}
	m.compChecking = true
	m.compMu.Unlock()
	go func() {
		st := m.checkOCR(model)
		m.compMu.Lock()
		m.components[1], m.compModel, m.compChecking = st, model, false
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
