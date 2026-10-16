package index

import "time"

// keepAwakeEvery、keepAwakeIdleFor、keepAwakeSet 测试时可以替换。
var (
	keepAwakeEvery = 5 * time.Second
	// keepAwakeIdleFor：没有任务这么久之后才恢复自动睡眠（两批文件之间、扫描刚结束时会短暂空闲，不来回切换）
	keepAwakeIdleFor = time.Minute
	keepAwakeSet     = setKeepAwake
)

// keepAwakeLoop：建立索引期间（有待处理的文件或正在扫描）、接通电源、没有暂停时，不让电脑自动睡眠。
// 电脑睡眠时所有程序都暂停，整夜无人操作时索引会停在睡眠前的位置。屏幕保护、关闭显示器不受影响；
// 索引全部完成、暂停或改用电池后立即恢复。只是向系统提出请求，不改电源设置；手动选择睡眠照常生效。
func (m *Manager) keepAwakeLoop() {
	defer m.wg.Done()
	if !keepAwakeSupported() {
		return
	}
	on := false
	defer func() {
		if on {
			_ = keepAwakeSet(false)
			m.keepingAwake.Store(false)
		}
	}()
	t := time.NewTicker(keepAwakeEvery)
	defer t.Stop()
	var lastBusy time.Time
	for {
		allowed := m.Config().KeepAwake && !m.pausedAll.Load() && !onBattery()
		if m.busy() {
			lastBusy = time.Now()
		}
		want := allowed && !lastBusy.IsZero() && time.Since(lastBusy) < keepAwakeIdleFor
		if want != on {
			if err := keepAwakeSet(want); err != nil {
				m.logf("设置「索引期间不让电脑自动睡眠」失败: %v", err)
			} else {
				on = want
				m.keepingAwake.Store(on)
				if on {
					m.logf("建立索引期间不让电脑自动睡眠（屏幕保护、关闭显示器不受影响；索引完成后恢复）")
				} else {
					m.logf("已恢复电脑自动睡眠")
				}
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// busy：有文件在处理或等待处理，或者正在扫描。
func (m *Manager) busy() bool {
	if !m.sched.idle.Load() {
		return true
	}
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	for _, s := range m.scanInfo {
		if s.running.Load() {
			return true
		}
	}
	return false
}
