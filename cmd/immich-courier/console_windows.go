package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:generate go run ../../tools/winicon -o winres/app.ico
//go:generate go tool rsrc -manifest winres/app.manifest -ico winres/app.ico -arch amd64 -o rsrc_windows_amd64.syso
//go:generate go tool rsrc -manifest winres/app.manifest -ico winres/app.ico -arch arm64 -o rsrc_windows_arm64.syso

func isWindows() bool { return true }
func isDarwin() bool  { return false }

// hideOwnConsole closes a console window that Windows opened just for this
// process. That happens on Windows versions older than 11 24H2, which ignore
// the "detached" console policy in the manifest. A console shared with a
// terminal the user started us from is left alone.
func hideOwnConsole() {
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd == 0 {
		return // no console at all (24H2+ with the manifest policy)
	}
	pids := make([]uint32, 4)
	n, err := getConsoleProcessList(pids)
	if err == nil && n == 1 {
		procFreeConsole.Call()
	}
}

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
)

func getConsoleProcessList(pids []uint32) (uint32, error) {
	r, _, err := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if r == 0 {
		return 0, err
	}
	return uint32(r), nil
}
