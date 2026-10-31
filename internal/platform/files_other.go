//go:build !darwin && !windows

package platform

import (
	"errors"
	"os/exec"
	"path/filepath"
)

func FreeSpace(dir string) (uint64, error) {
	return 0, errors.New("这个系统暂不支持查看剩余空间")
}

func MarkDownloaded(path string) error { return nil }

func OpenFolder(dir string) error { return exec.Command("xdg-open", dir).Start() }

func Reveal(path string) error { return exec.Command("xdg-open", filepath.Dir(path)).Start() }
