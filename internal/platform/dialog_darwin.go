package platform

import (
	"os/exec"
	"strings"
)

// ShowError 用系统的提示框显示错误。
func ShowError(title, msg string) {
	q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	exec.Command("osascript", "-e", "display alert "+q(title)+" message "+q(msg)+" as critical").Run()
}
