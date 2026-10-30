//go:build !darwin && !windows

package main

import "errors"

func autostartSupported() bool { return false }

func autostartEnabled() bool { return false }

func setAutostart(on bool, args []string) error {
	return errors.New("这个系统暂不支持开机启动")
}
