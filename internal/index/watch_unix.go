//go:build !windows

package index

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type fsnWatcher struct {
	w        *fsnotify.Watcher
	events   chan string
	overflow chan struct{}
	done     chan struct{}
	once     sync.Once
	skipDir  func(abs, name string) bool
}

func newRecursiveWatcher(root string, skipDir func(abs, name string) bool) (recursiveWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &fsnWatcher{w: w, events: make(chan string, 4096), overflow: make(chan struct{}, 1), done: make(chan struct{}), skipDir: skipDir}
	fw.addTree(root)
	go fw.loop()
	return fw, nil
}

func (f *fsnWatcher) addTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && f.skipDir(p, d.Name()) {
				return filepath.SkipDir
			}
			_ = f.w.Add(p)
		}
		return nil
	})
}

func (f *fsnWatcher) loop() {
	for {
		select {
		case <-f.done:
			return
		case ev, ok := <-f.w.Events:
			if !ok {
				return
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() && !f.skipDir(ev.Name, fi.Name()) {
					f.addTree(ev.Name)
				}
			}
			select {
			case f.events <- ev.Name:
			default:
				select {
				case f.overflow <- struct{}{}:
				default:
				}
			}
		case err, ok := <-f.w.Errors:
			if !ok {
				return
			}
			if err == fsnotify.ErrEventOverflow {
				select {
				case f.overflow <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (f *fsnWatcher) Events() <-chan string     { return f.events }
func (f *fsnWatcher) Overflow() <-chan struct{} { return f.overflow }
func (f *fsnWatcher) Close()                    { f.once.Do(func() { close(f.done); f.w.Close() }) }
