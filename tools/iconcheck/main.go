// tools/iconcheck: 校验 exe 内图标资源是否可按 MAKEINTRESOURCE(id) 加载；
// 附带 -win <pid> 模式：读取该进程顶层窗口的 WM_GETICON（运行时是否真的挂上了图标）。
// 用法: go run ./tools/iconcheck [exe] [id...]
//
//	go run ./tools/iconcheck -win <pid>
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	k32 = syscall.NewLazyDLL("kernel32.dll")
	u32 = syscall.NewLazyDLL("user32.dll")

	procLoadLibEx = k32.NewProc("LoadLibraryExW")
	procFreeLib   = k32.NewProc("FreeLibrary")
	procLoadImage = u32.NewProc("LoadImageW")
	procEnumWnd   = u32.NewProc("EnumWindows")
	procGetWndPID = u32.NewProc("GetWindowThreadProcessId")
	procSendMsg   = u32.NewProc("SendMessageW")
	procGetWndTxt = u32.NewProc("GetWindowTextW")
)

const (
	loadLibraryAsDatafile = 0x00000002
	imageIcon             = 1
	lrDefaultsize         = 0x40
	lrShared              = 0x8000
	wmGetIcon             = 0x007F
)

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "-win" {
		pid, _ := strconv.Atoi(os.Args[2])
		dumpWindowIcons(uint32(pid))
		return
	}
	exe := "dpsmeter.exe"
	ids := []int{1, 2, 3}
	if len(os.Args) > 1 {
		exe = os.Args[1]
	}
	if len(os.Args) > 2 {
		ids = nil
		for _, a := range os.Args[2:] {
			n, _ := strconv.Atoi(a)
			ids = append(ids, n)
		}
	}
	p, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	hmod, _, e := procLoadLibEx.Call(uintptr(unsafe.Pointer(p)), 0, loadLibraryAsDatafile)
	if hmod == 0 {
		fmt.Fprintln(os.Stderr, "LoadLibraryEx 失败:", e)
		os.Exit(1)
	}
	defer procFreeLib.Call(hmod)
	fmt.Printf("已作为数据文件加载 %s (hmod=%#x)\n", exe, hmod)

	for _, id := range ids {
		h, _, _ := procLoadImage.Call(hmod, uintptr(id), imageIcon, 0, 0, lrDefaultsize|lrShared)
		status := "缺失"
		if h != 0 {
			status = "OK"
		}
		fmt.Printf("  ID %-3d → %-4s hicon=%#x\n", id, status, h)
	}
}

// dumpWindowIcons 列出指定进程的顶层窗口并查询其 ICON_BIG / ICON_SMALL。
func dumpWindowIcons(pid uint32) {
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var wpid uint32
		procGetWndPID.Call(hwnd, uintptr(unsafe.Pointer(&wpid)))
		if wpid != pid {
			return 1
		}
		buf := make([]uint16, 256)
		procGetWndTxt.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		title := syscall.UTF16ToString(buf)
		big, _, _ := procSendMsg.Call(hwnd, wmGetIcon, 1, 0)
		small, _, _ := procSendMsg.Call(hwnd, wmGetIcon, 0, 0)
		fmt.Printf("hwnd=%#x title=%q ICON_BIG=%#x ICON_SMALL=%#x\n", hwnd, title, big, small)
		return 1
	})
	procEnumWnd.Call(cb, 0)
}
