package media

import (
	"path/filepath"
	"testing"
)

// 备份记录存下来以后重新读出来还在；原主人改名后刷新，别人的同名指纹不受影响；自己的和备份分得开。
func TestBackupsSaveAndRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backups.json")
	b := LoadBackups(path)
	b.Add("fp1", "A", "小A", VideoEntry{VideoInfo: VideoInfo{FP: "fp1", Name: "a.mp4"}, Group: "g", Version: 2, Origin: "x", Holders: []string{"y"}})

	b = LoadBackups(path)
	if !b.Has("fp1") || b.Has("fp2") {
		t.Fatal("重新读出来应该只有 fp1")
	}
	b.Refresh("B", "小B", []*VideoEntry{{VideoInfo: VideoInfo{FP: "fp1"}, Title: "不是原主人改的"}})
	b.Refresh("A", "小A（改名）", []*VideoEntry{{VideoInfo: VideoInfo{FP: "fp1", Name: "a.mp4"}, Title: "EP03", Group: "g", Version: 2}})
	own, backups := b.Split([]*VideoInfo{{FP: "fp1", Dir: "备份/小A"}, {FP: "fp3"}})
	if len(own) != 1 || own[0].FP != "fp3" || len(backups) != 1 {
		t.Fatalf("应该分成一个自己的、一个备份：%v %v", own, backups)
	}
	e := b.Entries(backups)[0]
	if e.Title != "EP03" || e.Origin != "A" || e.OriginName != "小A（改名）" || e.Version != 2 || len(e.Holders) != 0 || e.Dir != "" {
		t.Fatalf("备份的列表项应该照原主人最新的名称和版本：%+v", e)
	}
	if !IsVideoName("x.MOV") || IsVideoName("x.mxf") {
		t.Error("IsVideoName 不对")
	}
}
