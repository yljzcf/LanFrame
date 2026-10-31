package platform

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>
#include <stdlib.h>

// trash 把文件移到废纸篓，成功返回 NULL，失败返回原因（用完要 free）。
static char *trash(const char *path) {
	@autoreleasepool {
		NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:path]];
		NSError *err = nil;
		if ([[NSFileManager defaultManager] trashItemAtURL:url resultingItemURL:nil error:&err]) return NULL;
		const char *msg = err.localizedDescription.UTF8String;
		return strdup(msg ? msg : "未知原因");
	}
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// TrashName 是系统里废纸篓的叫法，提示里用。
const TrashName = "废纸篓"

// Trash 把文件或文件夹移到废纸篓，和在访达里删除一样，可以"放回原处"。
func Trash(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	if msg := C.trash(p); msg != nil {
		defer C.free(unsafe.Pointer(msg))
		return errors.New(C.GoString(msg))
	}
	return nil
}
