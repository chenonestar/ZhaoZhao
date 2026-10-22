package sysinfo

import (
	"encoding/binary"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// storagePropertyQuery 是 STORAGE_PROPERTY_QUERY（标准查询）。
type storagePropertyQuery struct {
	PropertyID uint32
	QueryType  uint32
	Extra      [1]byte
}

const (
	ioctlStorageQueryProperty = 0x2D1400
	storageDeviceProperty     = 0 // STORAGE_DEVICE_DESCRIPTOR：总线类型
	storageSeekPenalty        = 7 // DEVICE_SEEK_PENALTY_DESCRIPTOR：有没有寻道开销（机械硬盘）
)

// DiskMedia 说明路径所在的磁盘是固态硬盘还是机械硬盘，以及接口（NVMe、SATA、USB 等），如「机械硬盘，SATA」。
// 取不到（网络路径、虚拟盘、驱动不支持）时返回空字符串。不需要管理员权限。
func DiskMedia(p string) string {
	vol := filepath.VolumeName(p)
	if len(vol) != 2 || vol[1] != ':' {
		return ""
	}
	name, err := windows.UTF16PtrFromString(`\\.\` + vol)
	if err != nil {
		return ""
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	query := func(id uint32, out []byte) uint32 {
		q := storagePropertyQuery{PropertyID: id}
		var n uint32
		if windows.DeviceIoControl(h, ioctlStorageQueryProperty, (*byte)(unsafe.Pointer(&q)), uint32(unsafe.Sizeof(q)),
			&out[0], uint32(len(out)), &n, nil) != nil {
			return 0
		}
		return n
	}
	media := ""
	var seek [12]byte // Version、Size、IncursSeekPenalty
	if query(storageSeekPenalty, seek[:]) >= 9 {
		media = "固态硬盘"
		if seek[8] != 0 {
			media = "机械硬盘"
		}
	}
	var dev [64]byte // STORAGE_DEVICE_DESCRIPTOR 的前一段：总线类型在第 28 字节
	if query(storageDeviceProperty, dev[:]) >= 32 {
		if bus := busName(binary.LittleEndian.Uint32(dev[28:32])); bus != "" {
			if media != "" {
				media += "，"
			}
			media += bus
		}
	}
	return media
}

// busName 是 STORAGE_BUS_TYPE 的名称（常见的几种）。
func busName(t uint32) string {
	switch t {
	case 1:
		return "SCSI"
	case 3:
		return "ATA"
	case 7:
		return "USB"
	case 0x0A:
		return "SAS"
	case 0x0B:
		return "SATA"
	case 0x0C:
		return "SD 卡"
	case 0x0E, 0x0F:
		return "虚拟磁盘"
	case 0x11:
		return "NVMe"
	}
	return ""
}
