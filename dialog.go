// dialog.go —— 系统文件选择框（comdlg32 GetOpenFileNameW，零外部依赖）。
// 导入入口从 Go 侧弹框而非 WebView2 JS 文件选择：overlay 窗口无边框置顶，
// JS 端 input[type=file] 行为不可靠，且桥可服务浏览器/headless 多形态。
package main

import (
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	comdlg32             = syscall.NewLazyDLL("comdlg32.dll")
	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
	procCommDlgError     = comdlg32.NewProc("CommDlgExtendedError")
)

const (
	ofnExplorer      = 0x00080000
	ofnPathMustExist = 0x00000800
	ofnFileMustExist = 0x00001000
	ofnHideReadOnly  = 0x00000004
)

// openFileNameW 与 Win64 OPENFILENAMEW 逐字段对齐（lStructSize 必须 = 152）。
// 显式填充仅在 uint32 字段后跟 8 对齐指针字段且自然未对齐处（nMaxFile /
// nMaxFileTitle 之后）；其余间隙由 Go 自动补齐，多加一处即 STRUCTSIZE 错。
type openFileNameW struct {
	LStructSize       uint32 // 0
	_                 [4]byte
	HwndOwner         uintptr // 8
	HInstance         uintptr // 16
	LpstrFilter       uintptr // 24
	LpstrCustomFilter uintptr // 32
	NMaxCustFilter    uint32  // 40
	NFilterIndex      uint32  // 44
	LpstrFile         uintptr // 48（自然对齐）
	NMaxFile          uint32  // 56
	_                 [4]byte
	LpstrFileTitle    uintptr // 64
	NMaxFileTitle     uint32  // 72
	_                 [4]byte
	LpstrInitialDir   uintptr // 80
	LpstrTitle        uintptr // 88
	Flags             uint32  // 96
	NFileOffset       uint16  // 100
	NFileExtension    uint16  // 102
	LpstrDefExt       uintptr // 104（自然对齐）
	LCustData         uintptr // 112
	LpfnHook          uintptr // 120
	LpTemplateName    uintptr // 128
	PvReserved        uintptr // 136
	DwReserved        uint32  // 144
	FlagsEx           uint32  // 148 —— 总长 152，尾部不加填充
}

func init() {
	if n := unsafe.Sizeof(openFileNameW{}); n != 152 {
		panic(fmt.Sprintf("openFileNameW 布局错误：sizeof=%d，期望 152", n))
	}
}

// utf16Sep 各段以 NUL 分隔、双 NUL 结尾的 UTF-16 串（对话框 filter 格式）。
func utf16Sep(parts ...string) []uint16 {
	var out []uint16
	for _, s := range parts {
		out = append(out, utf16.Encode([]rune(s))...)
		out = append(out, 0)
	}
	return append(out, 0)
}

// utf16Opt 空串转 nil 指针（可选参数），否则转 UTF-16 指针。
func utf16Opt(s string) uintptr {
	if s == "" {
		return 0
	}
	return utf16p(s)
}

// pickOpenFile 弹出「打开文件」对话框。
// filter 形如 [描述, 模式, 描述2, 模式2...]；owner 为属主窗口（0 可无属主）。
// 返回所选绝对路径；用户取消返回 ("", nil)；出错返回 ("", err)。
func pickOpenFile(owner uintptr, title, initialDir string, filterPairs ...string) (string, error) {
	filter := utf16Sep(filterPairs...)
	fileBuf := make([]uint16, 32768) // MAX_PATH 稳妥上限
	title16, _ := syscall.UTF16FromString(title)
	ofn := openFileNameW{
		LStructSize:     uint32(unsafe.Sizeof(openFileNameW{})),
		HwndOwner:       owner,
		LpstrFilter:     uintptr(unsafe.Pointer(&filter[0])),
		NFilterIndex:    1,
		LpstrFile:       uintptr(unsafe.Pointer(&fileBuf[0])),
		NMaxFile:        uint32(len(fileBuf)),
		LpstrInitialDir: utf16Opt(initialDir),
		LpstrTitle:      uintptr(unsafe.Pointer(&title16[0])),
		Flags:           ofnExplorer | ofnPathMustExist | ofnFileMustExist | ofnHideReadOnly,
	}
	r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r != 0 {
		return syscall.UTF16ToString(fileBuf), nil
	}
	// 返回 0：CommDlgExtendedError()==0 即用户取消，非 0 是真实错误
	code, _, _ := procCommDlgError.Call()
	if code == 0 {
		return "", nil
	}
	return "", fmt.Errorf("文件选择框错误（CommDlgExtendedError=%d）", code)
}
