//go:build !darwin && !windows

package platform

func SetKeepAwake(on bool) {}
