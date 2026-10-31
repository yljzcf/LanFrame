package store

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 共享文件夹里的三个子文件夹（0.4.0 起）：
//   - 批注：所有批注记录和截图，电脑之间自动同步，不要手动删改
//   - 分享：自己要给同事看的视频和文件
//   - 备份：从同事那里拿来的东西，按来源分子文件夹；里面的视频是原主人视频的备份
//
// 0.3.x 及以前批注文件夹叫"批注同步(别删)"，视频和文件直接放在共享文件夹里，更新后自动整理。
// "分享"只是放东西的约定：扫描视频、给同事浏览文件时仍然看整个共享文件夹，
// 没挪成的、以后误放在根目录的照样能分享。
const (
	SyncDirName       = "批注"
	LegacySyncDirName = "批注同步(别删)"
	OutDirName        = "分享"
	BackupDirName     = "备份"
)

// IsSyncDir 判断共享文件夹根目录下的这个名字是不是批注文件夹（新旧名字都算）。
func IsSyncDir(name string) bool { return name == SyncDirName || name == LegacySyncDirName }

// HiddenName 是不给同事看、也不整理的文件：隐藏文件、Office 的临时文件和 Windows 的系统文件。
func HiddenName(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$") {
		return true
	}
	switch strings.ToLower(name) {
	case "thumbs.db", "desktop.ini", "system volume information", "$recycle.bin", "$windows.~bt", "$windows.~ws", "$sysreset":
		return true
	}
	return false
}

// syncDirOf 是批注文件夹的路径。旧名字的还在（没整理完，或者改名失败）就先照常用旧的。
func syncDirOf(shareDir string) string {
	if st, err := os.Stat(filepath.Join(shareDir, LegacySyncDirName)); err == nil && st.IsDir() {
		return filepath.Join(shareDir, LegacySyncDirName)
	}
	return filepath.Join(shareDir, SyncDirName)
}

// isOurSyncDir 判断这个"批注"文件夹是程序的（里面有 records），还是用户自己恰好起了这个名字。
func isOurSyncDir(path string) bool {
	st, err := os.Stat(filepath.Join(path, "records"))
	return err == nil && st.IsDir()
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// movable 判断共享文件夹根目录下的这一项要不要挪进"分享"。
// 用户原来就有的"分享"文件夹直接当成程序的；旧结构（0.3.x）里还没有程序的"备份"，
// 叫"备份"的是用户自己的文件夹，挪进"分享"，不和拿来的东西混在一起。
func movable(shareDir, name string) bool {
	switch {
	case name == OutDirName || name == LegacySyncDirName || HiddenName(name):
		return false
	case name == BackupDirName:
		return isDir(filepath.Join(shareDir, LegacySyncDirName))
	case name == SyncDirName:
		return !isOurSyncDir(filepath.Join(shareDir, name))
	}
	return true
}

// MovableCount 是整理时会挪进"分享"的项数，换共享文件夹前给用户确认用。系统文件夹不挪，返回 0。
func MovableCount(shareDir string) int {
	if SystemFolder(shareDir) != "" {
		return 0
	}
	entries, _ := os.ReadDir(shareDir)
	n := 0
	for _, e := range entries {
		if movable(shareDir, e.Name()) {
			n++
		}
	}
	return n
}

// SystemFolder 判断共享文件夹是不是个人文件夹、桌面这类系统文件夹或者整个磁盘，是的话返回它的名字。
// 这些文件夹里的东西不自动挪，免得把整个桌面挪走。
func SystemFolder(dir string) string {
	dir = filepath.Clean(dir)
	if filepath.Dir(dir) == dir || (runtime.GOOS == "darwin" && filepath.Dir(dir) == "/Volumes") {
		return "整个磁盘"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	same := func(p string) bool { return strings.EqualFold(filepath.Clean(p), dir) }
	if same(home) {
		return "个人文件夹"
	}
	for _, f := range []struct{ name, label string }{
		{"Desktop", "桌面"}, {"Documents", "文稿"}, {"Downloads", "下载"}, {"Movies", "影片"},
		{"Videos", "视频"}, {"Pictures", "图片"}, {"Music", "音乐"},
	} {
		// Windows 上桌面、文档常被 OneDrive 接管
		if same(filepath.Join(home, f.name)) || same(filepath.Join(home, "OneDrive", f.name)) {
			return "\"" + f.label + "\""
		}
	}
	return ""
}

// Layout 是整理共享文件夹的结果，给网页提示用。
type Layout struct {
	Migrated bool     // 从旧结构（0.3.x）整理过来的
	Moved    int      // 挪进"分享"的项数
	Failed   []string // 没挪成的
	Skipped  string   // 共享文件夹是系统文件夹，没有挪
}

// Note 是给用户看的整理结果；没什么要说的时候是空的。
func (l Layout) Note() string {
	var parts []string
	if l.Moved > 0 {
		parts = append(parts, fmt.Sprintf("原有的 %d 项已经挪进\"%s\"，剪辑软件工程里引用了这些文件的，需要重新链接素材（批注不受影响）。", l.Moved, OutDirName))
	}
	if len(l.Failed) > 0 {
		parts = append(parts, fmt.Sprintf("有 %d 项没挪成（可能正被别的程序打开），下次打开程序时再试，在那之前照样分享。", len(l.Failed)))
	}
	if l.Skipped != "" {
		parts = append(parts, fmt.Sprintf("共享文件夹是%s，原有的文件没有挪动，照样分享；新文件请放进\"%s\"。", l.Skipped, OutDirName))
	}
	return strings.Join(parts, "")
}

// PrepareShare 把共享文件夹整理成三个子文件夹。从旧结构更新上来时（有"批注同步(别删)"），
// 或者 move 为真时（用户换了共享文件夹并确认过），把根目录下的其他东西挪进"分享"；系统文件夹不挪。
// 都挪完以后才把"批注同步(别删)"改名为"批注"，没挪完的下次再试。共享文件夹本身不存在时不替用户建。
func PrepareShare(shareDir string, move bool) (Layout, error) {
	var res Layout
	if st, err := os.Stat(shareDir); err != nil {
		return res, err
	} else if !st.IsDir() {
		return res, os.ErrNotExist
	}
	legacy := filepath.Join(shareDir, LegacySyncDirName)
	if isDir(legacy) {
		res.Migrated, move = true, true
	}
	out := filepath.Join(shareDir, OutDirName)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return res, err
	}
	// 程序的"备份"等挪完再建：旧结构里用户自己的"备份"文件夹要先挪走
	defer func() {
		if err := os.MkdirAll(filepath.Join(shareDir, BackupDirName), 0o755); err != nil {
			log.Printf("建\"%s\"文件夹失败：%v", BackupDirName, err)
		}
	}()
	if move {
		if res.Skipped = SystemFolder(shareDir); res.Skipped == "" {
			entries, err := os.ReadDir(shareDir)
			if err != nil {
				return res, err
			}
			for _, e := range entries {
				if !movable(shareDir, e.Name()) {
					continue
				}
				target := freeName(out, e.Name(), e.IsDir())
				if err := os.Rename(filepath.Join(shareDir, e.Name()), target); err != nil {
					log.Printf("没能把 %s 挪进\"%s\"：%v", e.Name(), OutDirName, err)
					res.Failed = append(res.Failed, e.Name())
					continue
				}
				res.Moved++
			}
			if res.Moved > 0 {
				log.Printf("把共享文件夹里原有的 %d 项挪进了\"%s\"", res.Moved, OutDirName)
			}
		}
	}
	if res.Migrated && len(res.Failed) == 0 {
		target := filepath.Join(shareDir, SyncDirName)
		if _, err := os.Stat(target); err == nil {
			// 系统文件夹里用户自己有一个"批注"文件夹，没挪走它，就继续用旧名字
			log.Printf("共享文件夹里已经有\"%s\"，批注文件夹继续用旧名字\"%s\"", SyncDirName, LegacySyncDirName)
		} else if err := os.Rename(legacy, target); err != nil {
			log.Printf("批注文件夹改名失败，下次再试：%v", err)
		} else {
			log.Printf("批注文件夹从\"%s\"改名为\"%s\"", LegacySyncDirName, SyncDirName)
		}
	}
	return res, nil
}

// freeName 在 dir 里给 name 找一个没被占用的名字：已经有同名的就叫"名字 (1).扩展名"。
func freeName(dir, name string, isDir bool) string {
	ext := ""
	if !isDir {
		ext = filepath.Ext(name)
	}
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		p := filepath.Join(dir, n)
		if _, err := os.Lstat(p); err != nil {
			return p
		}
	}
}
