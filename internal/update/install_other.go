//go:build !darwin && !windows

package update

import (
	"errors"
	"os/exec"
)

func canInstall(exe string) error { return errors.New("这个系统不支持自动更新") }

func install(exe, pkg string) (string, error) { return "", canInstall(exe) }

func cleanup(exe string) {}

func StartNew(exe string, args []string) error { return exec.Command(exe, args...).Start() }
