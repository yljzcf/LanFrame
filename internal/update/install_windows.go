package update

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Windows 上运行中的 .exe 不能覆盖、不能删除，但可以改名：
// 新版本先复制成 .exe.new，旧的改名为 .exe.old，再把新的改成原来的名字。.old 等下次启动再删。
// 路径不变，开机启动项和防火墙规则都照样有效；程序自己下载的文件没有"来自网络"的标记，打开时不会弹 SmartScreen。

func canInstall(exe string) error {
	f, err := os.CreateTemp(filepath.Dir(exe), ".lan-video-review-check-")
	if err != nil {
		return fmt.Errorf("没有权限修改程序所在的文件夹 %s", filepath.Dir(exe))
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

func install(exe, pkg string) (string, error) {
	if err := canInstall(exe); err != nil {
		return "", err
	}
	tmp, old := exe+".new", exe+".old"
	if err := copyFile(pkg, tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("复制新版本失败：%v", err)
	}
	os.Remove(old) // 上上次更新留下的
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("替换程序失败：%v", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe)
		os.Remove(tmp)
		return "", fmt.Errorf("替换程序失败：%v", err)
	}
	return exe, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// cleanup 删掉上次更新留下的旧程序。旧进程刚退出时可能还删不掉，下次启动再删。
func cleanup(exe string) {
	os.Remove(exe + ".old")
	os.Remove(exe + ".new")
}

// StartNew 启动新程序。Windows 上子进程本来就不随父进程退出。
func StartNew(exe string, args []string) error {
	return exec.Command(exe, args...).Start()
}
