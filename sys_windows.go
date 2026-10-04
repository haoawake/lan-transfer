//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	shell32                  = syscall.NewLazyDLL("shell32.dll")
	ole32                    = syscall.NewLazyDLL("ole32.dll")
	procSetConsoleTitleW     = kernel32.NewProc("SetConsoleTitleW")
	procGetConsoleMode       = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode       = kernel32.NewProc("SetConsoleMode")
	procSetFileAttributesW   = kernel32.NewProc("SetFileAttributesW")
	procShellExecuteW        = shell32.NewProc("ShellExecuteW")
	procSHGetKnownFolderPath = shell32.NewProc("SHGetKnownFolderPath")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

func initConsole() {
	if p, err := syscall.UTF16PtrFromString(appName); err == nil {
		procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
	// 关掉控制台的「快速编辑模式」。开着的话，在窗口里随手点一下就会进入选中状态，
	// 程序往窗口里打印时整个卡住——包括正在进行的传输。
	h, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return
	}
	const enableQuickEdit, enableExtendedFlags = 0x0040, 0x0080
	procSetConsoleMode.Call(uintptr(h), uintptr(mode&^enableQuickEdit|enableExtendedFlags))
}

// downloadsDir 用系统 API 取「下载」文件夹：很多人把它挪到了 D 盘
func downloadsDir() string {
	// FOLDERID_Downloads {374DE290-123F-4565-9164-39C4925E467B}
	guid := syscall.GUID{Data1: 0x374DE290, Data2: 0x123F, Data3: 0x4565, Data4: [8]byte{0x91, 0x64, 0x39, 0xC4, 0x92, 0x5E, 0x46, 0x7B}}
	var p *uint16
	r, _, _ := procSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(&guid)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r == 0 && p != nil {
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
		if d := utf16PtrToString(p); d != "" {
			return d
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

func utf16PtrToString(p *uint16) string {
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; n++ {
		ptr = unsafe.Add(ptr, 2)
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

// shellOpen 用默认程序打开文件、文件夹或网址，相当于在资源管理器里双击
func shellOpen(target string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, 1)
	if r <= 32 {
		return fmt.Errorf("ShellExecute 返回 %d", r)
	}
	return nil
}

func openURL(u string) {
	if shellOpen(u) != nil {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	}
}

// revealFile 打开资源管理器并选中这个文件
func revealFile(path string) error {
	cmd := exec.Command("explorer.exe")
	// explorer 的参数格式特殊，必须是 /select,"路径"，不能让 Go 替它加引号
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	return cmd.Start()
}

func hideFile(path string) {
	if p, err := syscall.UTF16PtrFromString(path); err == nil {
		const fileAttributeHidden = 0x2
		procSetFileAttributesW.Call(uintptr(unsafe.Pointer(p)), fileAttributeHidden)
	}
}

func isDiskFull(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == 112 || errno == 39 // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	}
	return false
}
