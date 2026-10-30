package update

import (
	"os"
	"path/filepath"
)

// current 是正在运行的程序文件的真实路径。
func current() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return exe, nil
}

// CanInstall 判断能不能原地替换正在运行的程序，不能时返回原因（用户能照着做的话）。
func CanInstall() error {
	exe, err := current()
	if err != nil {
		return err
	}
	return canInstall(exe)
}

// Install 用安装包 pkg 替换正在运行的程序，返回新程序的路径。正在运行的进程不受影响，要另外启动新程序。
func Install(pkg string) (string, error) {
	exe, err := current()
	if err != nil {
		return "", err
	}
	return install(exe, pkg)
}

// Cleanup 清理上次更新留下的旧文件。
func Cleanup() {
	if exe, err := current(); err == nil {
		cleanup(exe)
	}
}
