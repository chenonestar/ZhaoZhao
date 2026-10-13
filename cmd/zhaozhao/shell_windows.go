//go:build windows

package main

import (
	"log"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/chenonestar/zhaozhao/internal/index"
	"github.com/chenonestar/zhaozhao/internal/server"
	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessage      = user32.NewProc("PostMessageW")
	procSendMessage      = user32.NewProc("SendMessageW")
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procLoadIcon         = user32.NewProc("LoadIconW")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenu       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procShellNotifyIcon  = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle  = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

const (
	wmDestroy      = 0x0002
	wmCommand      = 0x0111
	wmHotkey       = 0x0312
	wmDeviceChange = 0x0219
	wmUser         = 0x0400
	wmTray         = wmUser + 1
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmLDblClk      = 0x0203
	wmQuitShell    = wmUser + 2
	wmSetHotkey    = wmUser + 3

	nimAdd     = 0
	nimDelete  = 2
	nifMessage = 1
	nifIcon    = 2
	nifTip     = 4

	mfString    = 0
	mfSeparator = 0x0800
	tpmRightBtn = 0x0002
	tpmReturn   = 0x0100 // TrackPopupMenu 直接返回所选菜单项的编号（不再发送 WM_COMMAND）

	idOpen   = 1
	idPause  = 2
	idManage = 3
	idQuit   = 4

	hotkeyID = 1
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type notifyIconData struct {
	size            uint32
	hwnd            windows.Handle
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            windows.Handle
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	timeout         uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guid            windows.GUID
	balloonIcon     windows.Handle
}

type point struct{ x, y int32 }

type msg struct {
	hwnd    windows.Handle
	message uint32
	wparam  uintptr
	lparam  uintptr
	time    uint32
	pt      point
}

type shellCtx struct {
	URL       string
	Mgr       *index.Manager
	Hotkey    string
	Quit      func()
	OpenPanel func(panel string) // 呼出界面并打开指定面板（如「索引与设置」）
}

var (
	shellHwnd uintptr
	curShell  *shellCtx
)

// shutdownShell 从其他线程请求结束消息循环。
func shutdownShell() {
	if shellHwnd != 0 {
		procPostMessage.Call(shellHwnd, wmQuitShell, 0, 0)
		return
	}
	stopOnce.Do(func() { close(stopCh) }) // 没能创建隐藏窗口时，runShell 只是在等待退出
}

// parseHotkey 解析 "Ctrl+Alt+F" 形式的快捷键。
func parseHotkey(s string) (mods uint32, key uint32, ok bool) {
	for _, p := range strings.Split(s, "+") {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "ctrl", "control":
			mods |= 2
		case "alt":
			mods |= 1
		case "shift":
			mods |= 4
		case "win", "super":
			mods |= 8
		case "":
		default:
			k := strings.ToUpper(strings.TrimSpace(p))
			switch {
			case k == "SPACE" || k == "空格":
				key = 0x20
			case len(k) == 1 && ((k[0] >= 'A' && k[0] <= 'Z') || (k[0] >= '0' && k[0] <= '9')):
				key = uint32(k[0])
			case len(k) >= 2 && k[0] == 'F':
				n := 0
				for _, c := range k[1:] {
					if c < '0' || c > '9' {
						return 0, 0, false
					}
					n = n*10 + int(c-'0')
				}
				if n < 1 || n > 24 {
					return 0, 0, false
				}
				key = uint32(0x70 + n - 1)
			default:
				return 0, 0, false
			}
		}
	}
	// 至少要有 Ctrl、Alt 或 Win 之一：否则会抢走普通打字用的按键
	return mods, key, key != 0 && mods&(1|2|8) != 0
}

func wndProc(hwnd, m, wparam, lparam uintptr) uintptr {
	c := curShell
	switch uint32(m) {
	case wmHotkey:
		if c != nil {
			_ = showOrLaunchUI(c.URL)
		}
		return 0
	case wmTray:
		switch uint32(lparam & 0xFFFF) {
		case wmLButtonUp, wmLDblClk:
			if c != nil {
				_ = showOrLaunchUI(c.URL)
			}
		case wmRButtonUp:
			showMenu(hwnd, c)
		}
		return 0
	case wmCommand:
		trayCommand(c, wparam&0xFFFF)
		return 0
	case wmSetHotkey:
		hotkeyMu.Lock()
		spec := hotkeyWant
		hotkeyMu.Unlock()
		registerHotkey(hwnd, spec)
		return 0
	case wmDeviceChange:
		// 磁盘接入或移除：立即重新识别卷，而不是等下一次轮询
		if (wparam == 0x8000 || wparam == 0x8004) && c != nil && c.Mgr != nil {
			go c.Mgr.RefreshVolumes()
		}
		return 1
	case wmQuitShell, wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, m, wparam, lparam)
	return r
}

func showMenu(hwnd uintptr, c *shellCtx) {
	menu, _, _ := procCreatePopupMenu.Call()
	defer procDestroyMenu.Call(menu)
	add := func(id uintptr, text string) {
		t, _ := windows.UTF16PtrFromString(text)
		procAppendMenu.Call(menu, mfString, id, uintptr(unsafe.Pointer(t)))
	}
	add(idOpen, "打开找找")
	add(idManage, "索引与设置")
	pause := "暂停索引"
	if c != nil && c.Mgr != nil && c.Mgr.IsPaused() {
		pause = "恢复索引"
	}
	add(idPause, pause)
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
	add(idQuit, "退出找找")
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd) // 否则点击菜单外部时菜单不会消失
	// TPM_RETURNCMD：所选菜单项的编号作为返回值，必须在这里处理（v0.1.2 及以前忽略了返回值，菜单项全部无效）
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightBtn|tpmReturn, uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
	procPostMessage.Call(hwnd, 0, 0, 0)
	if cmd != 0 {
		trayCommand(c, cmd)
	}
}

// trayCommand 执行托盘菜单命令。
func trayCommand(c *shellCtx, id uintptr) {
	if c == nil {
		return
	}
	names := map[uintptr]string{idOpen: "打开找找", idManage: "索引与设置", idPause: "暂停/恢复索引", idQuit: "退出找找"}
	log.Printf("托盘菜单：%s", names[id])
	switch id {
	case idOpen:
		_ = showOrLaunchUI(c.URL)
	case idManage:
		if c.OpenPanel != nil {
			c.OpenPanel("index")
		} else {
			_ = showOrLaunchUI(c.URL)
		}
	case idPause:
		if c.Mgr != nil {
			if c.Mgr.IsPaused() {
				c.Mgr.ResumeAll()
			} else {
				c.Mgr.PauseAll()
			}
		}
	case idQuit:
		if c.Quit != nil {
			c.Quit()
		}
	}
}

// ---- 全局快捷键：在界面线程（托盘窗口所属线程）上注册，设置修改后立即生效 ----

var (
	hotkeyMu    sync.Mutex
	hotkeyWant  string
	hotkeyState = server.HotkeyState{Message: "未启用"}
	hotkeyOn    bool
)

// registerHotkey 注销旧的快捷键并注册新的。只能在托盘窗口所属的线程调用。
func registerHotkey(hwnd uintptr, spec string) {
	hotkeyMu.Lock()
	defer hotkeyMu.Unlock()
	if hotkeyOn {
		procUnregisterHotKey.Call(hwnd, hotkeyID)
		hotkeyOn = false
	}
	spec = strings.TrimSpace(spec)
	st := server.HotkeyState{Hotkey: spec}
	switch mods, key, ok := parseHotkey(spec); {
	case spec == "":
		st.Message = "未启用"
	case !ok:
		st.Message = "无法识别这个组合，至少要包含 Ctrl、Alt 或 Win 之一，再加一个字母、数字、F1–F24 或空格"
	default:
		if r, _, err := procRegisterHotKey.Call(hwnd, hotkeyID, uintptr(mods|0x4000), uintptr(key)); r == 0 {
			st.Message = "被其他程序占用，请换一个组合"
			log.Printf("注册全局快捷键 %s 失败（被其他程序占用）: %v", spec, err)
		} else {
			hotkeyOn, st.Active, st.Message = true, true, "已生效"
			log.Printf("全局快捷键 %s 已生效", spec)
		}
	}
	hotkeyState = st
}

// applyHotkey 从其他线程修改全局快捷键，返回结果。
func applyHotkey(spec string) server.HotkeyState {
	if shellHwnd == 0 {
		return server.HotkeyState{Hotkey: spec, Message: "托盘尚未就绪"}
	}
	hotkeyMu.Lock()
	hotkeyWant = spec
	hotkeyMu.Unlock()
	procSendMessage.Call(shellHwnd, wmSetHotkey, 0, 0) // 同步：返回时已在界面线程上处理完
	return currentHotkey()
}

func currentHotkey() server.HotkeyState {
	hotkeyMu.Lock()
	defer hotkeyMu.Unlock()
	return hotkeyState
}

// runShell 创建隐藏窗口，提供托盘图标、全局快捷键和磁盘变化通知，并运行消息循环直到退出。
func runShell(c *shellCtx) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	curShell = c

	inst, _, _ := procGetModuleHandle.Call(0)
	cls, _ := windows.UTF16PtrFromString("ZhaoZhaoShell")
	icon, _, _ := procLoadIcon.Call(0, 32512) // IDI_APPLICATION
	wc := wndClassEx{
		size: uint32(unsafe.Sizeof(wndClassEx{})), wndProc: syscall.NewCallback(wndProc),
		instance: windows.Handle(inst), icon: windows.Handle(icon), className: cls, iconSm: windows.Handle(icon),
	}
	if r, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		log.Printf("注册窗口类失败: %v", err)
		waitOnly()
		return
	}
	title, _ := windows.UTF16PtrFromString("ZhaoZhaoShellWindow")
	hwnd, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		log.Printf("创建窗口失败: %v", err)
		waitOnly()
		return
	}
	shellHwnd = hwnd

	nid := notifyIconData{size: uint32(unsafe.Sizeof(notifyIconData{})), hwnd: windows.Handle(hwnd), id: 1,
		flags: nifMessage | nifIcon | nifTip, callbackMessage: wmTray, icon: windows.Handle(icon)}
	tip, _ := windows.UTF16FromString("找找")
	copy(nid.tip[:], tip)
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	defer procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))

	registerHotkey(hwnd, c.Hotkey)
	defer procUnregisterHotKey.Call(hwnd, hotkeyID)

	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

var (
	stopCh   = make(chan struct{})
	stopOnce sync.Once
)

func waitOnly() { <-stopCh }
