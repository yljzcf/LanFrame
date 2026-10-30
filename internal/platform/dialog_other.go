//go:build !darwin && !windows

package platform

func ShowError(title, msg string) {}
