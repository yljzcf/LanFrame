package platform

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TrashName 是系统里回收站的叫法，提示里用。
const TrashName = "回收站"

var procSHFileOperation = windows.NewLazySystemDLL("shell32.dll").NewProc("SHFileOperationW")

// shFileOpStruct 对应 SHFILEOPSTRUCTW。只编译 64 位，按自然对齐，和 Go 的布局一样。
type shFileOpStruct struct {
	hwnd          uintptr
	wFunc         uint32
	from          *uint16
	to            *uint16
	flags         uint16
	anyAborted    int32
	nameMappings  uintptr
	progressTitle *uint16
}

const (
	foDelete           = 3
	fofSilent          = 0x4
	fofNoConfirmation  = 0x10
	fofAllowUndo       = 0x40
	fofNoErrorUI       = 0x400
	fofWantNukeWarning = 0x4000 // 太大、回收站放不下时，先问要不要直接删除，不悄悄删掉
)

// Trash 把文件或文件夹移到回收站，和在资源管理器里删除一样，可以还原。
func Trash(path string) error {
	from, err := windows.UTF16FromString(path)
	if err != nil {
		return err
	}
	from = append(from, 0) // 文件列表以两个 0 结尾
	op := shFileOpStruct{
		wFunc: foDelete, from: &from[0],
		flags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI | fofWantNukeWarning,
	}
	r, _, _ := procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
	switch {
	case op.anyAborted != 0:
		return errors.New("已取消")
	case r != 0:
		return fmt.Errorf("错误码 %#x", r)
	}
	return nil
}
