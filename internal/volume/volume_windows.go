//go:build windows

package volume

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type systemProvider struct{}

// System 返回基于 Windows 卷序列号 + 卷 GUID 的提供者。
func System() Provider { return systemProvider{} }

var (
	mpr                  = windows.NewLazySystemDLL("mpr.dll")
	procWNetGetConnectio = mpr.NewProc("WNetGetConnectionW")
)

// uncOf 返回映射盘符对应的 UNC 路径（\\server\share），不是映射盘返回空串。
func uncOf(drive string) string {
	name, _ := windows.UTF16PtrFromString(drive) // 形如 "Z:"
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procWNetGetConnectio.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func infoForDrive(letter string) (Info, error) {
	root := letter + `\`
	rootp, _ := windows.UTF16PtrFromString(root)
	dt := windows.GetDriveType(rootp)
	if dt == windows.DRIVE_REMOTE {
		unc := uncOf(letter)
		if unc == "" {
			return Info{}, fmt.Errorf("无法确定网络驱动器 %s 的共享路径", letter)
		}
		return Info{ID: "unc:" + strings.ToLower(unc), Label: unc, Kind: KindNetwork, Mount: root}, nil
	}
	var label [windows.MAX_PATH + 1]uint16
	var serial, maxComp, flags uint32
	var fsName [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformation(rootp, &label[0], uint32(len(label)), &serial, &maxComp, &flags, &fsName[0], uint32(len(fsName))); err != nil {
		return Info{}, err
	}
	var guid [windows.MAX_PATH + 1]uint16
	_ = windows.GetVolumeNameForVolumeMountPoint(rootp, &guid[0], uint32(len(guid)))
	g := windows.UTF16ToString(guid[:])
	g = strings.TrimSuffix(strings.TrimPrefix(g, `\\?\Volume`), `\`)
	var free, total, totalFree uint64
	_ = windows.GetDiskFreeSpaceEx(rootp, &free, &total, &totalFree)
	kind := KindLocal
	if dt == windows.DRIVE_REMOVABLE {
		kind = KindRemovable
	}
	lbl := windows.UTF16ToString(label[:])
	if lbl == "" {
		lbl = letter
	}
	return Info{ID: fmt.Sprintf("vol:%08X:%s", serial, strings.ToLower(g)), Label: lbl, Serial: fmt.Sprintf("%08X", serial),
		GUID: g, Capacity: int64(total), Kind: kind, Mount: root}, nil
}

func (systemProvider) Detect(path string) (Info, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Info{}, "", err
	}
	if strings.HasPrefix(abs, `\\`) {
		parts := strings.Split(strings.TrimPrefix(abs, `\\`), `\`)
		if len(parts) < 2 {
			return Info{}, "", fmt.Errorf("无效的网络路径 %s", path)
		}
		unc := `\\` + parts[0] + `\` + parts[1]
		rel := strings.Join(parts[2:], "/")
		return Info{ID: "unc:" + strings.ToLower(unc), Label: unc, Kind: KindNetwork, Mount: unc + `\`}, rel, nil
	}
	if len(abs) < 2 || abs[1] != ':' {
		return Info{}, "", fmt.Errorf("无法识别路径所在的磁盘：%s", path)
	}
	info, err := infoForDrive(abs[:2])
	if err != nil {
		return Info{}, "", err
	}
	rel := strings.Trim(filepath.ToSlash(abs[2:]), "/")
	return info, rel, nil
}

func (systemProvider) List() []Info {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []Info
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + ":"
		// 避免对未插入介质的光驱等触发弹窗
		old := windows.SetErrorMode(windows.SEM_FAILCRITICALERRORS)
		info, err := infoForDrive(letter)
		windows.SetErrorMode(old)
		if err == nil {
			out = append(out, info)
		}
	}
	return out
}

var _ = syscall.Errno(0)
