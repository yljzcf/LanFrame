package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 覆盖导出时，旧文件没有批注就直接顶替；有批注才问"是不是新版本"。
func TestSamePathReplace(t *testing.T) {
	vs := loadVersions(filepath.Join(t.TempDir(), "versions.json"))
	p1 := &VideoInfo{FP: "p1", Name: "LAX43_EP03.mp4", ModTime: 1}
	vs.Update([]*VideoInfo{p1}, nil)
	p2 := &VideoInfo{FP: "p2", Name: "LAX43_EP03.mp4", ModTime: 2}
	vs.Update([]*VideoInfo{p2}, nil)
	list := vs.Listing([]*VideoInfo{p2}, nil)
	if len(list) != 1 || list[0].Suggest != "" || list[0].Versions != 1 {
		t.Fatalf("没有批注的旧文件应该被直接顶替：%+v", list)
	}
	p3 := &VideoInfo{FP: "p3", Name: "LAX43_EP03.mp4", ModTime: 3}
	vs.Update([]*VideoInfo{p3}, map[string]bool{"p2": true})
	list = vs.Listing([]*VideoInfo{p3}, map[string]bool{"p2": true})
	if len(list) != 2 || list[1].Suggest == "" {
		t.Fatalf("旧文件有批注时应该提示是不是新版本：%+v", list)
	}
}

// 正在导出、拷贝的文件要等写完才算进来。
func TestScanWaitsForFinishedFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, age time.Duration) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, make([]byte, size), 0o644)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
	}
	write("旧文件.mp4", 100, 2*time.Hour)
	write("刚写完.mp4", 100, 20*time.Second)
	write("空文件.mp4", 0, 2*time.Hour)
	l := newVideoLib()
	names := func() map[string]bool {
		out := map[string]bool{}
		for _, v := range l.List() {
			out[v.Name] = true
		}
		return out
	}
	l.Scan(dir)
	if got := names(); !got["旧文件.mp4"] || got["刚写完.mp4"] || got["空文件.mp4"] {
		t.Fatalf("第一次扫描：%v", got)
	}
	l.Scan(dir)
	if got := names(); !got["刚写完.mp4"] || got["空文件.mp4"] {
		t.Fatalf("第二次扫描，大小和时间没变应该算写完了：%v", got)
	}
}

func TestBaseKey(t *testing.T) {
	same := [][2]string{
		{"宣传片_v1.mp4", "宣传片_v2.mp4"},
		{"demo.mp4", "demo (1).mp4"},
		{"预告 第2版.mp4", "预告.mov"},
		{"Trailer FINAL.mp4", "trailer-v3.mp4"},
		{"LAX43_EP03_v01.mp4", "LAX43_EP03_v02.mov"},
		{"LAX43_EP03.mp4", "LAX43_EP03_v02.mp4"},
	}
	for _, p := range same {
		if baseKey(p[0]) != baseKey(p[1]) {
			t.Errorf("%s 和 %s 应该算同一个片子", p[0], p[1])
		}
	}
	different := [][2]string{
		{"shot01.mp4", "shot02.mp4"},
		{"MV01.mp4", "MV02.mp4"},
		{"LAX43_EP03_v02.mp4", "LAX43_EP04_v02.mp4"},
	}
	for _, p := range different {
		if baseKey(p[0]) == baseKey(p[1]) {
			t.Errorf("%s 和 %s 不应该算同一个片子", p[0], p[1])
		}
	}
}

func TestVersionName(t *testing.T) {
	vs := loadVersions(filepath.Join(t.TempDir(), "versions.json"))
	a := &VideoInfo{FP: "a", Name: "粗剪.mp4", ModTime: 1}
	vs.Update([]*VideoInfo{a}, nil)
	g := vs.Listing([]*VideoInfo{a}, nil)[0].Group
	if err := vs.SetName(g, "LAX43_EP03"); err != nil {
		t.Fatal(err)
	}
	// 文件名和旧文件不像，但和视频名称一样，也要提示是新版本
	b := &VideoInfo{FP: "b", Name: "LAX43_EP03_v02.mp4", ModTime: 2}
	vs.Update([]*VideoInfo{a, b}, nil)
	list := vs.Listing([]*VideoInfo{a, b}, nil)
	if list[0].Title != "LAX43_EP03" || list[1].Suggest != "LAX43_EP03" {
		t.Fatalf("名称或新版本提示不对：%+v %+v", list[0], list[1])
	}
	vs.Answer("b", true)
	members, _ := vs.Members(g)
	if len(members) != 2 || members[0].FP != "a" || members[1].FP != "b" {
		t.Fatalf("版本顺序不对：%+v", members)
	}
}

func TestVersionSuggestAndMove(t *testing.T) {
	vs := loadVersions(filepath.Join(t.TempDir(), "versions.json"))
	v1 := &VideoInfo{FP: "a", Name: "片子_v1.mp4", ModTime: 1}
	vs.Update([]*VideoInfo{v1}, nil)
	// 覆盖导出：旧文件不在了，出现一个指纹不同的新文件
	v2 := &VideoInfo{FP: "b", Name: "片子_v2.mp4", ModTime: 2}
	vs.Update([]*VideoInfo{v2}, map[string]bool{"a": true})

	list := vs.Listing([]*VideoInfo{v2}, map[string]bool{"a": true})
	if len(list) != 2 || list[1].Suggest != "片子_v1.mp4" {
		t.Fatalf("应该提示 v2 是 v1 的新版本：%+v", list)
	}
	if err := vs.Answer("b", true); err != nil {
		t.Fatal(err)
	}
	list = vs.Listing([]*VideoInfo{v2}, map[string]bool{"a": true})
	if len(list) != 2 || list[0].Group != list[1].Group || !list[0].Missing || list[1].Version != 2 || list[1].Suggest != "" {
		t.Fatalf("确认后应该在同一组，v1 标为文件已不在：%+v %+v", list[0], list[1])
	}
	// 没有批注的旧版本文件不在了就不显示
	if got := vs.Listing([]*VideoInfo{v2}, nil); len(got) != 1 {
		t.Fatalf("没有批注的旧版本不该显示：%d", len(got))
	}
	// 手动拆出来
	if err := vs.Move("b", ""); err != nil {
		t.Fatal(err)
	}
	list = vs.Listing([]*VideoInfo{v2}, map[string]bool{"a": true})
	if list[0].Group == list[1].Group {
		t.Fatal("拆开后应该是两个视频")
	}
	// 拒绝过的不再提示，重新加载后也一样
	vs2 := loadVersions(vs.path)
	vs2.Update([]*VideoInfo{v2}, map[string]bool{"a": true})
	for _, e := range vs2.Listing([]*VideoInfo{v2}, nil) {
		if e.Suggest != "" {
			t.Fatal("处理过的文件不应该再提示")
		}
	}
}
