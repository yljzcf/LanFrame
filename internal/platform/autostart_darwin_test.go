package platform

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 用临时目录当 HOME，不会碰到真正的 ~/Library/LaunchAgents。
func TestLaunchAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if AutostartEnabled() {
		t.Fatal("一开始不应该有启动项")
	}
	if err := SetAutostart(true, []string{"-child", "-autostart", "-data", "/tmp/有 空格 & 符号"}); err != nil {
		t.Fatal(err)
	}
	if !AutostartEnabled() {
		t.Fatal("应该已经开启")
	}
	data, _ := os.ReadFile(autostartPath())
	if !strings.Contains(string(data), "<string>/tmp/有 空格 &amp; 符号</string>") || !strings.Contains(string(data), "<key>RunAtLoad</key>") {
		t.Fatalf("启动项内容不对：\n%s", data)
	}
	if out, err := exec.Command("plutil", "-lint", autostartPath()).CombinedOutput(); err != nil {
		t.Fatalf("plist 格式不对：%s", out)
	}
	if err := SetAutostart(false, nil); err != nil || AutostartEnabled() {
		t.Fatalf("关闭失败：%v", err)
	}
	if err := SetAutostart(false, nil); err != nil {
		t.Fatalf("重复关闭不应该报错：%v", err)
	}
}
