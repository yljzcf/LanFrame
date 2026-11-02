package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 文件名的号偏小时"改正"文件名；起名并改文件名时，用每个版本自己的号。
func TestVersionFileNames(t *testing.T) {
	a, err := newApp(t.TempDir(), 28380)
	if err != nil {
		t.Fatal(err)
	}
	share := t.TempDir()
	if err := a.saveConfig(Config{Nickname: "测试", ShareDir: share, Paused: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.openShare(share, false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(share, "分享")
	write := func(name, content string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	// X_v05 较旧，X_v02 较新：X_v02 编成 v06
	write("X_v05.mp4", "五", 2*time.Hour)
	write("X_v02.mp4", "二", time.Hour)
	a.rescan()
	entry := func(name string) (fp string, version, fileVer int, group string) {
		for _, e := range a.listing() {
			if e.Name == name {
				return e.FP, e.Version, e.FileVer, e.Group
			}
		}
		t.Fatalf("列表里没有 %s", name)
		return
	}
	fp2, _, _, _ := entry("X_v02.mp4")
	if err := a.getVersions().Answer(fp2, true); err != nil {
		t.Fatalf("应该提示 X_v02 是 X_v05 的新版本：%v", err)
	}
	if _, v, fv, _ := entry("X_v02.mp4"); v != 6 || fv != 2 {
		t.Fatalf("X_v02 应该编成 v06（文件名里是 v02），实际 v%d / %d", v, fv)
	}

	// 目标文件名被占用时不改
	write("X_v06.mp4", "别的文件", 3*time.Hour)
	if _, err := a.fixVersionName(fp2); err == nil {
		t.Fatal("X_v06.mp4 已经有了，不应该改")
	}
	os.Remove(filepath.Join(dir, "X_v06.mp4"))
	a.rescan()

	name, err := a.fixVersionName(fp2)
	if err != nil || name != "X_v06.mp4" {
		t.Fatalf("改正失败：%q %v", name, err)
	}
	fp6, v, fv, group := entry("X_v06.mp4")
	if fp6 != fp2 || v != 6 || fv != 6 {
		t.Fatalf("改正后应该是同一个文件、v06：%s v%d / %d", fp6, v, fv)
	}

	// 起名并改文件名：保留每个版本自己的号
	if failed, err := a.renameVideo(group, "LAX", true); err != nil || len(failed) > 0 {
		t.Fatalf("改名失败：%v %v", failed, err)
	}
	for _, n := range []string{"LAX_v05.mp4", "LAX_v06.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("应该有 %s", n)
		}
	}
}
