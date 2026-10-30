package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeApp 在 dir 里做一个 .app，里面的"程序"是一个内容为 content 的可执行文件。
func fakeApp(t *testing.T, dir, name, content string) string {
	exe := filepath.Join(dir, name, "Contents", "MacOS", "lan-video-review")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestInstallDarwin(t *testing.T) {
	root := t.TempDir()
	apps := filepath.Join(root, "Applications")
	// 用户把程序改过名，更新后应该保留这个名字
	exe := fakeApp(t, apps, "我的审片.app", "旧版本")

	src := filepath.Join(root, "src")
	fakeApp(t, src, "局域网审片.app", "新版本")
	zip := filepath.Join(root, "new.zip")
	if out, err := exec.Command("ditto", "-c", "-k", "--keepParent", filepath.Join(src, "局域网审片.app"), zip).CombinedOutput(); err != nil {
		t.Fatalf("打包失败：%v %s", err, out)
	}

	got, err := install(exe, zip)
	if err != nil {
		t.Fatal(err)
	}
	if got != exe {
		t.Fatalf("新程序路径是 %s，应该是 %s", got, exe)
	}
	if data, _ := os.ReadFile(exe); string(data) != "新版本" {
		t.Fatalf("程序内容是 %q，应该已经换成新版本", data)
	}
	if st, _ := os.Stat(exe); st.Mode()&0o111 == 0 {
		t.Fatal("新程序应该能执行")
	}
	if entries, _ := os.ReadDir(apps); len(entries) != 1 {
		t.Fatalf("应用程序文件夹里只应该有一个 .app，现在有 %d 个", len(entries))
	}

	// 坏的更新包：原来的程序保持不动
	bad := filepath.Join(root, "bad.zip")
	os.WriteFile(bad, []byte("不是 zip"), 0o644)
	if _, err := install(exe, bad); err == nil {
		t.Fatal("坏的更新包应该安装失败")
	}
	if data, _ := os.ReadFile(exe); string(data) != "新版本" {
		t.Fatal("安装失败时原来的程序不应该被改动")
	}
	if entries, _ := os.ReadDir(apps); len(entries) != 1 {
		t.Fatal("安装失败时不应该留下临时文件夹")
	}
}

func TestCanInstallDarwin(t *testing.T) {
	if err := canInstall("/usr/local/bin/lan-video-review"); err == nil {
		t.Error("不在 .app 里的程序不能自动更新")
	}
	if err := canInstall("/private/var/folders/x/AppTranslocation/ABC/d/局域网审片.app/Contents/MacOS/lan-video-review"); err == nil {
		t.Error("被系统挪到随机位置运行的程序不能自动更新")
	}
	exe := fakeApp(t, t.TempDir(), "局域网审片.app", "")
	if err := canInstall(exe); err != nil {
		t.Errorf("可以写的文件夹里应该能更新：%v", err)
	}
}
