package platform

import (
	"runtime"
	"syscall"
)

var awakeCh = make(chan bool)

// SetThreadExecutionState 只对调用它的线程有效，所以固定在一个一直不退出的线程上调用。
func init() {
	go func() {
		runtime.LockOSThread()
		proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")
		const esContinuous, esSystemRequired = 0x80000000, 0x00000001
		for on := range awakeCh {
			if on {
				proc.Call(uintptr(esContinuous | esSystemRequired))
			} else {
				proc.Call(uintptr(esContinuous))
			}
		}
	}()
}

// SetKeepAwake 运行服务期间阻止 Windows 空闲睡眠，暂停服务时取消。
func SetKeepAwake(on bool) { awakeCh <- on }
