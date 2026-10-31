package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"lanreview/internal/store"
)

// writeShareFile 在 a 的共享文件夹里放一个文件，rel 用 / 分隔。
func writeShareFile(t *testing.T, a *App, rel, content string) string {
	t.Helper()
	p := filepath.Join(a.config().ShareDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// peerGet 模拟局域网里另一台电脑上的程序来要东西。
func peerGet(a *App, target string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://192.168.1.20:28380"+target, nil)
	r.RemoteAddr = "192.168.1.30:50000"
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func filesQuery(rel string, deep bool) string {
	q := "/peer/files?path=" + url.QueryEscape(rel)
	if deep {
		q += "&deep=1"
	}
	return q
}

// 共享文件夹里的文件都能列出来，隐藏文件、Office 临时文件、系统文件和批注同步文件夹不列。
func TestPeerFilesListing(t *testing.T) {
	a := newTestApp(t, "A")
	writeShareFile(t, a, "a.mp4", "video")
	writeShareFile(t, a, ".hidden", "x")
	writeShareFile(t, a, "Thumbs.db", "x")
	writeShareFile(t, a, "~$合同.docx", "x")
	writeShareFile(t, a, "素材/镜头1.mov", "shot")
	writeShareFile(t, a, "素材/.DS_Store", "x")
	writeShareFile(t, a, "素材/子/说明.txt", "readme")

	w := peerGet(a, filesQuery("", false), nil)
	var list []fileEntry
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("列文件失败：%d %s", w.Code, w.Body.String())
	}
	names := []string{}
	for _, f := range list {
		names = append(names, f.Name)
	}
	if len(list) != 4 || !list[0].Dir || !list[2].Dir || list[3].Path != "a.mp4" || list[3].Size != 5 ||
		!slices.Contains(names, "素材") || !slices.Contains(names, store.OutDirName) || !slices.Contains(names, store.BackupDirName) {
		t.Fatalf("根目录应该是三个文件夹（素材、分享、备份）和 a.mp4，文件夹在前：%v", names)
	}

	w = peerGet(a, filesQuery("素材", true), nil)
	list = nil
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 2 || list[0].Path != "素材/子/说明.txt" || list[1].Path != "素材/镜头1.mov" {
		t.Fatalf("展开 素材 应该是两个文件：%+v", list)
	}
	w = peerGet(a, filesQuery("a.mp4", true), nil)
	list = nil
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Path != "a.mp4" {
		t.Fatalf("展开一个文件就是它自己：%+v", list)
	}
	w = peerGet(a, filesQuery("", true), nil)
	list = nil
	json.Unmarshal(w.Body.Bytes(), &list)
	for _, f := range list {
		if f.Path == store.SyncDirName || filepath.Dir(filepath.FromSlash(f.Path)) == store.SyncDirName {
			t.Fatalf("批注同步文件夹不应该给同事下载：%+v", f)
		}
	}

	// 下载支持断点续传
	w = peerGet(a, "/peer/file?path="+url.QueryEscape("素材/镜头1.mov"), map[string]string{"Range": "bytes=2-"})
	if w.Code != http.StatusPartialContent || w.Body.String() != "ot" {
		t.Fatalf("从第 2 个字节接着下应该得到 ot：%d %q", w.Code, w.Body.String())
	}
}

// 共享文件夹以外的东西、隐藏文件和批注同步文件夹都拿不到；浏览器、口令不对、关了文件下载时一律拒绝。
func TestPeerFilesRejects(t *testing.T) {
	a := newTestApp(t, "A")
	writeShareFile(t, a, "a.mp4", "video")
	writeShareFile(t, a, ".hidden", "secret")
	writeShareFile(t, a, "素材/.DS_Store", "x")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	os.WriteFile(outside, []byte("secret"), 0o644)
	share := a.config().ShareDir
	if err := os.Symlink(outside, filepath.Join(share, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(share, "linkdir")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{
		"../config.json", "..", "素材/../../x", "/etc/passwd", outside, `..\x`,
		".hidden", "素材/.DS_Store", "link.txt", "linkdir/outside.txt",
		store.SyncDirName + "/versions.json", store.SyncDirName,
	} {
		w := peerGet(a, "/peer/file?path="+url.QueryEscape(p), nil)
		if w.Code == http.StatusOK || w.Code == http.StatusPartialContent {
			t.Errorf("%q 不应该能下载：%d %q", p, w.Code, w.Body.String())
		}
		w = peerGet(a, filesQuery(p, true), nil)
		if w.Code == http.StatusOK && w.Body.String() != "[]\n" {
			t.Errorf("%q 不应该能列出来：%q", p, w.Body.String())
		}
	}
	// 符号链接本身也不列出来
	w := peerGet(a, filesQuery("", false), nil)
	var list []fileEntry
	json.Unmarshal(w.Body.Bytes(), &list)
	for _, f := range list {
		if f.Name == "link.txt" || f.Name == "linkdir" {
			t.Errorf("符号链接不应该列出来：%+v", f)
		}
	}

	if w := peerGet(a, "/peer/file?path=a.mp4", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != http.StatusForbidden {
		t.Errorf("浏览器发来的请求应该拒绝：%d", w.Code)
	}

	c := a.config()
	c.TeamKey = "口令"
	a.saveConfig(c)
	if w := peerGet(a, "/peer/file?path=a.mp4", nil); w.Code != http.StatusForbidden {
		t.Errorf("没带团队口令应该拒绝：%d", w.Code)
	}
	if w := peerGet(a, "/peer/file?path=a.mp4", map[string]string{"X-Team-Key": teamToken("口令")}); w.Code != http.StatusOK {
		t.Errorf("带了正确的团队口令应该能下载：%d", w.Code)
	}

	c.TeamKey, c.NoFileShare = "", true
	a.saveConfig(c)
	if w := peerGet(a, "/peer/file?path=a.mp4", nil); w.Code != http.StatusForbidden {
		t.Errorf("关了文件下载应该拒绝：%d", w.Code)
	}
	if w := peerGet(a, filesQuery("", false), nil); w.Code != http.StatusForbidden {
		t.Errorf("关了文件下载应该拒绝列文件：%d", w.Code)
	}
	var info peerInfo
	json.Unmarshal(peerGet(a, "/peer/info", nil).Body.Bytes(), &info)
	if info.Files {
		t.Error("关了文件下载，/peer/info 里不应该说能下载")
	}
}
