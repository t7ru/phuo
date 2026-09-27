//go:build windows

package ui

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

const enableVirtualTerminalProcessing = 0x0004

func enableVT(f *os.File) {
	if !term.IsTerminal(int(f.Fd())) {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	var mode uint32
	h := f.Fd()
	r, _, _ := getMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return
	}
	mode |= enableVirtualTerminalProcessing
	setMode.Call(h, uintptr(mode))
}
