//go:build !windows

package index

var keepAwakeSupported = func() bool { return false }

func setKeepAwake(bool) error { return nil }
