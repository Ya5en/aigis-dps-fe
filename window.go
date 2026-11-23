// window.go —— overlay 形态：无边框置顶 WebView2 小窗 + 托盘控制 + 点击穿透。
// 默认锁定（点击穿透，不挡游戏操作）；托盘菜单或 Ctrl+Alt+L 切换穿透，
// Ctrl+Alt+H / 托盘 / 双击托盘图标 切换显隐；页面顶部拖条用于解锁后移动窗口。
// 构建 -H windowsgui 无控制台；窗口关闭（托盘退出 / Alt+F4）= 进程退出。
package main

import (
	"log"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// ---- Win32 常量 ----
const (
	gwlStyle   = ^uintptr(15) // -16

	wsCaption     = 0x00C00000
	wsThickframe  = 0x00040000
	wsSysmenu     = 0x00080000
	wsMinimizebox = 0x00020000
	wsMaximizebox = 0x00010000

	wsExToolwindow = 0x00000080 //（保留常量备用：置回后窗口从任务栏/Alt+Tab/捕获枚举隐身）

	swpNosize       = 0x0001
	swpNomove       = 0x0002
	swpNozorder     = 0x0004
	swpNoactivate   = 0x0010
	swpShowwin      = 0x0040
	swpFramechanged = 0x0020

	wmClose        = 0x0010
	wmDestroy      = 0x0002
	wmLbuttondblcl = 0x0203
	wmRbuttonup    = 0x0205
	wmHotkey       = 0x0312
	wmAppTray      = 0x8001

	nimAdd    = 0
	nimDelete = 2
	nifAll    = 0x1 | 0x2 | 0x4 // message | icon | tip

	modControl = 0x2
	modAlt     = 0x1

	mfString       = 0x0
	mfSeparator    = 0x800
	tpmRightbutton = 0x0002
	tpmReturNcmd   = 0x0100

	swHide = 0
	swShow = 5

	hwndTopmost = ^uintptr(0)

	iconApplication = 32512 // IDI_APPLICATION

	imageIcon     = 1 // IMAGE_ICON
	lrDefaultsize = 0x00000040
	lrShared      = 0x00008000
	smCxicon      = 11
	smCxsmicon    = 49
	smCysmicon    = 50
	wmSeticon     = 0x0080 // WM_SETICON；WPARAM: 0=ICON_SMALL 1=ICON_BIG

	iconResID = 1 // versioninfo.json → rsrc_windows_amd64.syso 的图标资源 ID（goversioninfo 约定 1）

	hkToggleShow = 2
)

var (
	u32     = syscall.NewLazyDLL("user32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")

	procGetWndLong   = u32.NewProc("GetWindowLongPtrW")
	procSetWndLong   = u32.NewProc("SetWindowLongPtrW")
	procSetWndPos    = u32.NewProc("SetWindowPos")
	procGetWndRect   = u32.NewProc("GetWindowRect")
	procShowWindow   = u32.NewProc("ShowWindow")
	procPostMsg      = u32.NewProc("PostMessageW")
	procSetFg        = u32.NewProc("SetForegroundWindow")
	procRegHotKey    = u32.NewProc("RegisterHotKey")
	procGetMsg       = u32.NewProc("GetMessageW")
	procTransMsg     = u32.NewProc("TranslateMessage")
	procDispMsg      = u32.NewProc("DispatchMessageW")
	procDefWndProc   = u32.NewProc("DefWindowProcW")
	procCreatePopup  = u32.NewProc("CreatePopupMenu")
	procAppendMenu   = u32.NewProc("AppendMenuW")
	procTrackPopup   = u32.NewProc("TrackPopupMenuEx")
	procDestroyMenu  = u32.NewProc("DestroyMenu")
	procLoadIcon     = u32.NewProc("LoadIconW")
	procLoadImage    = u32.NewProc("LoadImageW")
	procSendMsg      = u32.NewProc("SendMessageW")
	procGetSysMetric = u32.NewProc("GetSystemMetrics")
	procGetCursorPos = u32.NewProc("GetCursorPos")
	procRegClass     = u32.NewProc("RegisterClassExW")
	procCreateWnd    = u32.NewProc("CreateWindowExW")
	procDestroyWnd   = u32.NewProc("DestroyWindow")
	procPostQuit     = u32.NewProc("PostQuitMessage")
	procGetModHandle = k32.NewProc("GetModuleHandleW")
	procShellNotify  = shell32.NewProc("Shell_NotifyIconW")
)

type point struct{ X, Y int32 }

type rect struct{ L, T, R, B int32 }

type msgStruct struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type wndClassExW struct {
	CbSize       uint32
	Style        uint32
	LpfnWndProc  uintptr
	CbClsExtra   int32
	CbWndExtra   int32
	HInstance    uintptr
	HIcon        uintptr
	HCursor      uintptr
	HbrBackground uintptr
	LpszMenuName  uintptr
	LpszClassName uintptr
	HIconSm       uintptr
}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
}

// ---- overlay 状态 ----
var (
	wv        webview2.WebView
	hwndMain  uintptr
	hwndCtl   uintptr
	visible   atomic.Bool
	trayNid   notifyIconDataW
	trayAdded atomic.Bool
)

func utf16p(s string) uintptr {
	p, _ := syscall.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

// smallIconSize 返回当前 DPI 下的小图标尺寸（SM_CXSMICON / SM_CYSMICON）。
func smallIconSize() (int, int) {
	cx, _, _ := procGetSysMetric.Call(smCxsmicon)
	cy, _, _ := procGetSysMetric.Call(smCysmicon)
	return int(cx), int(cy)
}

// loadAppIcon 加载 exe 内嵌的图标资源（iconResID，由 versioninfo.json 生成 syso 嵌入）。
// cx/cy 都为 0 时取系统标准尺寸（LR_DEFAULTSIZE）；资源缺失回退 IDI_APPLICATION。
// 返回值带 LR_SHARED（进程内共享，无需 DestroyIcon）。
func loadAppIcon(hinstance uintptr, cx, cy int) uintptr {
	flag := uintptr(lrShared)
	if cx == 0 && cy == 0 {
		flag |= lrDefaultsize
	}
	h, _, _ := procLoadImage.Call(hinstance, iconResID, imageIcon, uintptr(cx), uintptr(cy), flag)
	if h != 0 {
		return h
	}
	h, _, _ = procLoadIcon.Call(0, iconApplication) // 回退：无 .syso 构建 / 资源异常时
	if h == 0 {
		log.Println("[!] 图标加载失败（资源缺失且回退 IDI_APPLICATION 失败）")
	}
	return h
}

// runOverlay 创建无边框置顶 overlay 窗口并运行消息泵；返回即退出。
func runOverlay(addr string, width, height int) {
	visible.Store(true)

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: false,
		WindowOptions: webview2.WindowOptions{
			Title:  "aigis-dps",
			Width:  uint(width),
			Height: uint(height),
			Center: true,
		},
	})
	if w == nil {
		log.Println("[!] WebView2 初始化失败（运行时未安装？）——可用 -headless + 浏览器访问面板")
		return
	}
	defer w.Destroy()
	defer trayDelete()

	wv = w
	hwndMain = uintptr(w.Window())

	// 主窗口图标（任务栏 / Alt+Tab）：WebView2 建的窗口类不带图标，手动 WM_SETICON。
	hinstance, _, _ := procGetModHandle.Call(0)
	procSendMsg.Call(hwndMain, wmSeticon, 1, loadAppIcon(hinstance, 0, 0)) // ICON_BIG
	scx, scy := smallIconSize()
	procSendMsg.Call(hwndMain, wmSeticon, 0, loadAppIcon(hinstance, scx, scy)) // ICON_SMALL

	_ = w.Bind("overlayMove", func(dx, dy float64) {
		moveWindow(hwndMain, int(dx), int(dy))
	})
	_ = w.Bind("overlayResize", func(h float64) {
		resizeWindowHeight(hwndMain, int(h))
	})
	_ = w.Bind("exitApp", func() {
		procPostMsg.Call(hwndMain, wmClose, 0, 0) // 与托盘退出同路径，defer 清理生效
	})

	applyBorderless()
	applyExStyle()
	w.Navigate("http://" + addr)

	go trayLoop()
	go keepTopmost()
	log.Printf("[*] overlay 已打开（无边框置顶；Ctrl+Alt+H 显隐，托盘菜单退出）")
	w.Run()
	log.Println("[*] overlay 已关闭，退出")
}

func applyBorderless() {
	st, _, _ := procGetWndLong.Call(hwndMain, uintptr(gwlStyle))
	st &^= wsCaption | wsThickframe | wsSysmenu | wsMinimizebox | wsMaximizebox
	procSetWndLong.Call(hwndMain, uintptr(gwlStyle), st)
	procSetWndPos.Call(hwndMain, 0, 0, 0, 0, 0,
		uintptr(swpNomove|swpNosize|swpNozorder|swpFramechanged|swpShowwin))
}

// applyExStyle 启动时重申置顶（SetWindowPos(HWND_TOPMOST) 才真正改 z 序；
// 不再用 WS_EX_TOOLWINDOW——它会同时把窗口从任务栏 / Alt+Tab / 捕获工具
// 枚举列表（OBS 等）里隐身，改为默认全部可见。
func applyExStyle() {
	procSetWndPos.Call(hwndMain, hwndTopmost, 0, 0, 0, 0,
		uintptr(swpNomove|swpNosize|swpNoactivate|swpShowwin))
	procSetWndPos.Call(hwndMain, 0, 0, 0, 0, 0,
		uintptr(swpNomove|swpNosize|swpNoactivate|swpFramechanged))
}

// keepTopmost 周期重申置顶（其他置顶窗口覆盖时压回最上层）。
func keepTopmost() {
	tick := time.NewTicker(5 * time.Second)
	for range tick.C {
		if hwndMain == 0 || !visible.Load() {
			continue
		}
		procSetWndPos.Call(hwndMain, hwndTopmost, 0, 0, 0, 0,
			uintptr(swpNomove|swpNosize|swpNoactivate))
	}
}

func toggleShow() {
	if visible.Load() {
		procShowWindow.Call(hwndMain, swHide)
		visible.Store(false)
	} else {
		procShowWindow.Call(hwndMain, swShow)
		procSetWndPos.Call(hwndMain, hwndTopmost, 0, 0, 0, 0,
			uintptr(swpNomove|swpNosize|swpShowwin|swpNoactivate))
		visible.Store(true)
	}
}

func moveWindow(hwnd uintptr, dx, dy int) {
	var r rect
	procGetWndRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	procSetWndPos.Call(hwnd, 0,
		uintptr(int32(r.L)+int32(dx)), uintptr(int32(r.T)+int32(dy)), 0, 0,
		uintptr(swpNosize|swpNozorder|swpNoactivate))
}

// resizeWindowHeight 自适应窗口高度（宽度不变、位置不动）。
// 无边框窗口无客户区偏移，窗口高即内容高；240~800px 保证可用且不遮屏。
func resizeWindowHeight(hwnd uintptr, h int) {
	const minH = 240
	const maxH = 800
	if h < minH {
		h = minH
	}
	if h > maxH {
		h = maxH
	}
	var r rect
	procGetWndRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	procSetWndPos.Call(hwnd, 0, 0, 0,
		uintptr(r.R-r.L), uintptr(h), // 保留当前宽度
		uintptr(swpNomove|swpNozorder|swpNoactivate))
}

// ---- 托盘 / 热键（独立 UI 线程）----

func trayLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hinstance, _, _ := procGetModHandle.Call(0)
	scx, scy := smallIconSize()
	hicon := loadAppIcon(hinstance, scx, scy)

	className := utf16p("aigisdps_ctl")
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		HInstance:     hinstance,
		LpszClassName: className,
		LpfnWndProc:   windows.NewCallback(ctlProc),
	}
	if _, _, er := procRegClass.Call(uintptr(unsafe.Pointer(&wc))); er.(syscall.Errno) != 0 {
		// ERROR_CLASS_ALREADY_EXISTS(1410) 时忽略（重开窗口场景）
		if er != syscall.Errno(1410) {
			log.Printf("[!] 托盘窗口类注册失败: %v", er)
		}
	}

	hwnd, _, _ := procCreateWnd.Call(0, className, utf16p("aigisdps_ctl"),
		0, 0, 0, 0, 0, 0, 0, hinstance, 0)
	hwndCtl = hwnd
	if hwnd == 0 {
		log.Println("[!] 托盘控制窗口创建失败")
		return
	}

	tip := "aigis-dps"
	nid := notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             hwnd,
		UID:              1,
		UFlags:           nifAll,
		UCallbackMessage: wmAppTray,
		HIcon:            hicon,
	}
	for i, r := range tip {
		nid.SzTip[i] = uint16(r)
	}
	trayNid = nid
	r, _, _ := procShellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&trayNid)))
	if r == 0 {
		log.Printf("[!] 托盘图标添加失败（Shell_NotifyIconW NIM_ADD）")
		return
	}
	trayAdded.Store(true)
	log.Println("[*] 托盘图标已添加")

	procRegHotKey.Call(hwnd, hkToggleShow, uintptr(modControl|modAlt), 0x48) // Ctrl+Alt+H 显隐

	var m msgStruct
	for {
		r, _, _ := procGetMsg.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 {
			return // WM_QUIT
		}
		procTransMsg.Call(uintptr(unsafe.Pointer(&m)))
		procDispMsg.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func ctlProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmHotkey:
		if wp == hkToggleShow {
			toggleShow()
		}
		return 0
	case wmAppTray:
		switch lp {
		case wmRbuttonup:
			trayMenu(hwnd)
		case wmLbuttondblcl:
			toggleShow()
		}
		return 0
	case wmDestroy:
		procPostQuit.Call(0)
		return 0
	}
	r, _, _ := procDefWndProc.Call(hwnd, msg, wp, lp)
	return r
}

func trayMenu(hwnd uintptr) {
	visLabel := "隐藏面板"
	if !visible.Load() {
		visLabel = "显示面板"
	}

	hm, _, _ := procCreatePopup.Call()
	procAppendMenu.Call(hm, mfString, 2, utf16p(visLabel))
	procAppendMenu.Call(hm, mfSeparator, 0, 0)
	procAppendMenu.Call(hm, mfString, 3, utf16p("退出"))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetFg.Call(hwnd)
	sel, _, _ := procTrackPopup.Call(hm, uintptr(tpmRightbutton|tpmReturNcmd),
		uintptr(pt.X), uintptr(pt.Y), hwnd, 0)
	procDestroyMenu.Call(hm)

	switch sel {
	case 2:
		toggleShow()
	case 3:
		procPostMsg.Call(hwndMain, wmClose, 0, 0)
	}
}

func trayDelete() {
	if !trayAdded.Load() {
		return
	}
	procShellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&trayNid)))
	trayAdded.Store(false)
}
