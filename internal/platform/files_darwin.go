package platform

import (
	"fmt"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// FreeSpace 是 dir 所在磁盘的剩余空间（字节）。
func FreeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// MarkDownloaded 给从别的电脑拿来的文件加上隔离标记，和浏览器、AirDrop 一样：
// 收到的程序第一次打开时系统会检查，不会直接运行。视频和素材不受影响。
func MarkDownloaded(path string) error {
	return unix.Setxattr(path, "com.apple.quarantine", []byte(fmt.Sprintf("0081;%08x;LanFrame;", time.Now().Unix())), 0)
}

// OpenFolder 在访达里打开这个文件夹。
func OpenFolder(dir string) error {
	return exec.Command("open", dir).Start()
}

// Reveal 在访达里显示并选中这个文件或文件夹。
func Reveal(path string) error {
	return exec.Command("open", "-R", path).Start()
}
