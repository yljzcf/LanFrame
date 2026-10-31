package platform

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// FreeSpace 是 dir 所在磁盘的剩余空间（字节）。
func FreeSpace(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}

// MarkDownloaded 给从别的电脑拿来的文件加上"来自其他计算机"的标记（Zone.Identifier），和浏览器下载的一样：
// 收到的程序打开时 SmartScreen 会检查，不会直接运行。视频和素材不受影响。
func MarkDownloaded(path string) error {
	return os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644)
}

// OpenFolder 在资源管理器里打开这个文件夹。
func OpenFolder(dir string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe "` + dir + `"`}
	return cmd.Start()
}

// Reveal 在资源管理器里显示并选中这个文件或文件夹。
// explorer 自己解析命令行，Go 默认的引号转义它认不对，所以整行自己拼。
func Reveal(path string) error {
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	return cmd.Start()
}
