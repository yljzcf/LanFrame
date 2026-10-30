//go:build !darwin

package main

// Windows 版编译成没有命令行窗口的程序，不需要转到后台。
func detachFromApp() bool { return false }
