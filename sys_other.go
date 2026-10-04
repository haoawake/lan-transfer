//go:build !windows

package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func initConsole() {
	// 设置终端窗口标题
	os.Stdout.WriteString("\033]0;" + appName + "\007")
}

func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	// Linux 桌面环境可能把「下载」改成了别的名字，比如中文系统里的 ~/下载
	if runtime.GOOS == "linux" {
		if f, err := os.Open(filepath.Join(home, ".config", "user-dirs.dirs")); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if v, ok := strings.CutPrefix(line, "XDG_DOWNLOAD_DIR="); ok {
					v = strings.Trim(v, `"`)
					v = strings.Replace(v, "$HOME", home, 1)
					if filepath.IsAbs(v) {
						return v
					}
				}
			}
		}
	}
	return filepath.Join(home, "Downloads")
}

func opener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func shellOpen(target string) error {
	return exec.Command(opener(), target).Start()
}

func openURL(u string) { _ = shellOpen(u) }

func revealFile(path string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", "-R", path).Start()
	}
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}

func hideFile(string) {} // 以点开头的文件夹本来就是隐藏的

// diskFree 返回 path 所在磁盘上当前用户还能用的空间
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
