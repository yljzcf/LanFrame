package platform

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// DetachFromApp：从 .app 双击启动时，在后台另起一个进程运行，自己马上退出。
// 这样 .app 不会一直显示为"正在运行"，再次双击时能重新启动，进而打开界面。
func DetachFromApp() bool {
	exe, err := os.Executable()
	if err != nil || !strings.HasSuffix(filepath.Dir(exe), ".app/Contents/MacOS") {
		return false
	}
	cmd := exec.Command(exe, append(os.Args[1:], "-child")...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("转到后台失败，直接运行：%v", err)
		return false
	}
	return true
}
