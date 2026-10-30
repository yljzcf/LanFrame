package platform

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

var (
	awakeMu    sync.Mutex
	caffeinate *exec.Cmd
)

// SetKeepAwake 运行服务期间阻止 Mac 空闲睡眠（合上笔记本盖子仍会睡眠），暂停服务时取消。
func SetKeepAwake(on bool) {
	awakeMu.Lock()
	defer awakeMu.Unlock()
	if on && caffeinate == nil {
		cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
		if err := cmd.Start(); err != nil {
			log.Printf("无法阻止睡眠：%v", err)
			return
		}
		go cmd.Wait()
		caffeinate = cmd
	} else if !on && caffeinate != nil {
		caffeinate.Process.Kill()
		caffeinate = nil
	}
}
