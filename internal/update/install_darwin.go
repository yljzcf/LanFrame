package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Mac 上替换整个 .app：在 .app 所在的文件夹里解压新版本，再把新旧两个 .app 换个位置。
// 运行中的程序文件被删掉后，进程照常运行到退出。

const stagePrefix = ".lan-video-review-update-"

// appPath 是程序所在的 .app。
func appPath(exe string) (string, error) {
	dir := filepath.Dir(exe)
	if !strings.HasSuffix(dir, ".app/Contents/MacOS") {
		return "", errors.New("程序不在 .app 里，不能自动更新")
	}
	return filepath.Dir(filepath.Dir(dir)), nil
}

func canInstall(exe string) error {
	app, err := appPath(exe)
	if err != nil {
		return err
	}
	// 从"下载"里直接双击、没有拖到别处的 .app，系统会把它放在一个只读的随机位置运行
	if strings.Contains(app, "/AppTranslocation/") {
		return errors.New(`程序是从"下载"文件夹里直接打开的，macOS 不允许它替换自己。请退出程序，把"局域网审片"拖进"应用程序"文件夹，再从那里打开`)
	}
	dir, err := os.MkdirTemp(filepath.Dir(app), stagePrefix)
	if err != nil {
		return fmt.Errorf("没有权限修改程序所在的文件夹 %s", filepath.Dir(app))
	}
	os.Remove(dir)
	return nil
}

func install(exe, pkg string) (string, error) {
	if err := canInstall(exe); err != nil {
		return "", err
	}
	app, _ := appPath(exe)
	stage, err := os.MkdirTemp(filepath.Dir(app), stagePrefix)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	// ditto 解压能保留可执行权限和签名
	if out, err := exec.Command("ditto", "-x", "-k", pkg, stage).CombinedOutput(); err != nil {
		return "", fmt.Errorf("解压更新包失败：%v %s", err, strings.TrimSpace(string(out)))
	}
	newApp := ""
	entries, _ := os.ReadDir(stage)
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			newApp = filepath.Join(stage, e.Name())
			break
		}
	}
	rel := strings.TrimPrefix(exe, app) // /Contents/MacOS/lan-video-review
	if st, err := os.Stat(newApp + rel); newApp == "" || err != nil || st.Mode()&0o111 == 0 {
		return "", errors.New("更新包里找不到程序")
	}
	old := filepath.Join(stage, "old.app")
	if err := os.Rename(app, old); err != nil {
		return "", fmt.Errorf("替换程序失败：%v", err)
	}
	if err := os.Rename(newApp, app); err != nil {
		os.Rename(old, app)
		return "", fmt.Errorf("替换程序失败：%v", err)
	}
	return app + rel, nil
}

// cleanup 删掉上次更新中途退出时留下的临时文件夹。
func cleanup(exe string) {
	app, err := appPath(exe)
	if err != nil {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(app), stagePrefix+"*"))
	for _, d := range dirs {
		os.RemoveAll(d)
	}
}

// StartNew 启动新程序，并和当前进程脱离关系：当前进程退出后它继续运行。
func StartNew(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
