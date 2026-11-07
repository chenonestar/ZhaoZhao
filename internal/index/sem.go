package index

import (
	"context"
	"sync"
)

// sem 是可以在运行中调整容量的计数信号量（老格式转换、OCR、网络盘读取的名额）。
type sem struct {
	mu   sync.Mutex
	cap  int
	used int
	wake chan struct{} // 名额释放或容量变化时关闭并换新，唤醒所有等待者重新检查
}

func newSem(n int) *sem {
	if n < 1 {
		n = 1
	}
	return &sem{cap: n, wake: make(chan struct{})}
}

// TryAcquire 立即尝试占用一个名额，不等待。
func (s *sem) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used < s.cap {
		s.used++
		return true
	}
	return false
}

// Acquire 等待一个名额，ctx 取消时返回 false。
func (s *sem) Acquire(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if s.used < s.cap {
			s.used++
			s.mu.Unlock()
			return true
		}
		w := s.wake
		s.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return false
		}
	}
}

func (s *sem) Release() {
	s.mu.Lock()
	s.used--
	s.broadcast()
	s.mu.Unlock()
}

// SetCap 调整容量。缩小时已占用的名额照常归还，只是暂时不再发放新的。
func (s *sem) SetCap(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	s.cap = n
	s.broadcast()
	s.mu.Unlock()
}

// Free 说明当前是否有空闲名额。
func (s *sem) Free() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used < s.cap
}

// Stats 返回已占用数和容量。
func (s *sem) Stats() (used, cap int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used, s.cap
}

func (s *sem) broadcast() {
	close(s.wake)
	s.wake = make(chan struct{})
}
