//go:build !darwin && !windows

package platform

import "errors"

func AutostartSupported() bool { return false }

func AutostartEnabled() bool { return false }

func SetAutostart(on bool, args []string) error {
	return errors.New("这个系统暂不支持开机启动")
}
