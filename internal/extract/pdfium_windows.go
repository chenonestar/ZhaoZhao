//go:build windows

package extract

import "golang.org/x/sys/windows"

// openLibrary 加载 DLL：用完整路径加载，并让它依赖的 DLL 也从同一目录查找。
func openLibrary(path string) (uintptr, error) {
	h, err := windows.LoadLibraryEx(path, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
	return uintptr(h), err
}
