package store

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// 从 0.3.x 更新上来：原有的视频、文件夹（包括用户自己叫"批注""备份"的文件夹）挪进"分享"，
// 隐藏文件留在原地，"批注同步(别删)"改名为"批注"，批注还在。第二次不再挪。
func TestPrepareShareMigrates(t *testing.T) {
	share := t.TempDir()
	touch(t, filepath.Join(share, LegacySyncDirName, "records", "x.txt"), "")
	touch(t, filepath.Join(share, LegacySyncDirName, "versions.json"), "{}")
	touch(t, filepath.Join(share, "a.mp4"), "a")
	touch(t, filepath.Join(share, "素材", "b.mov"), "b")
	touch(t, filepath.Join(share, SyncDirName, "笔记.txt"), "用户自己的批注文件夹")
	touch(t, filepath.Join(share, BackupDirName, "工程备份.zip"), "用户自己的备份文件夹")
	touch(t, filepath.Join(share, ".DS_Store"), "")
	touch(t, filepath.Join(share, "~$合同.docx"), "")

	res, err := PrepareShare(share, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Migrated || res.Moved != 4 || len(res.Failed) != 0 || res.Skipped != "" || res.Note() == "" {
		t.Fatalf("应该从旧结构整理过来，挪 4 项：%+v", res)
	}
	out := filepath.Join(share, OutDirName)
	for _, p := range []string{"a.mp4", filepath.Join("素材", "b.mov"), filepath.Join(SyncDirName, "笔记.txt"), filepath.Join(BackupDirName, "工程备份.zip")} {
		if !exists(filepath.Join(out, p)) {
			t.Errorf("%s 应该挪进了\"分享\"", p)
		}
	}
	if !exists(filepath.Join(share, SyncDirName, "versions.json")) || exists(filepath.Join(share, LegacySyncDirName)) {
		t.Error("\"批注同步(别删)\"应该改名为\"批注\"")
	}
	if !exists(filepath.Join(share, ".DS_Store")) || !exists(filepath.Join(share, "~$合同.docx")) {
		t.Error("隐藏文件和 Office 临时文件不应该挪")
	}
	if entries, err := os.ReadDir(filepath.Join(share, BackupDirName)); err != nil || len(entries) != 0 {
		t.Error("应该建好程序自己的、空的\"备份\"")
	}

	res, _ = PrepareShare(share, false)
	if res.Migrated || res.Moved != 0 {
		t.Fatalf("已经整理过，不应该再挪：%+v", res)
	}
	st, err := Open(share)
	if err != nil || st.Dir() != filepath.Join(share, SyncDirName) {
		t.Fatalf("批注文件夹应该是\"批注\"：%v %v", st, err)
	}
}

// 换了一个新的共享文件夹并确认过（move）时，原有的东西也挪进"分享"，同名的另起名字；没确认时不挪。
func TestPrepareShareNewFolder(t *testing.T) {
	share := t.TempDir()
	touch(t, filepath.Join(share, "a.mp4"), "新的")
	touch(t, filepath.Join(share, OutDirName, "a.mp4"), "已有的")
	if n := MovableCount(share); n != 1 {
		t.Fatalf("应该有 1 项要挪：%d", n)
	}
	if res, _ := PrepareShare(share, false); res.Moved != 0 || !exists(filepath.Join(share, "a.mp4")) {
		t.Fatalf("没确认时不挪：%+v", res)
	}
	res, err := PrepareShare(share, true)
	if err != nil || res.Moved != 1 || res.Migrated {
		t.Fatalf("确认后应该挪 1 项：%+v %v", res, err)
	}
	if data, _ := os.ReadFile(filepath.Join(share, OutDirName, "a (1).mp4")); string(data) != "新的" {
		t.Errorf("同名的应该存成 a (1).mp4：%q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(share, OutDirName, "a.mp4")); string(data) != "已有的" {
		t.Errorf("已有的不应该被覆盖：%q", data)
	}
}

// 共享文件夹是桌面这类系统文件夹时，原有的东西不挪，免得把整个桌面挪走；批注文件夹照样改名。
func TestPrepareShareSystemFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	share := filepath.Join(home, "Desktop")
	touch(t, filepath.Join(share, LegacySyncDirName, "records", "x.txt"), "")
	touch(t, filepath.Join(share, "a.mp4"), "a")
	res, err := PrepareShare(share, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != "\"桌面\"" || res.Moved != 0 || !exists(filepath.Join(share, "a.mp4")) {
		t.Fatalf("桌面里的东西不应该挪：%+v", res)
	}
	if !exists(filepath.Join(share, SyncDirName, "records")) {
		t.Error("批注文件夹应该照样改名")
	}
	if MovableCount(share) != 0 || SystemFolder(home) != "个人文件夹" {
		t.Error("系统文件夹不应该算要挪的")
	}
}
