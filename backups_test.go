package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lanreview/internal/media"
	"lanreview/internal/store"
)

// localPost 模拟本机网页发来的修改请求。
func localPost(t *testing.T, a *App, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://localhost:28380"+target, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func findEntry(list []*media.VideoEntry, fp string) *media.VideoEntry {
	for _, e := range list {
		if e.FP == fp {
			return e
		}
	}
	return nil
}

// A 分享一个视频并写了批注；B 把它拿过来，成为 A 的视频的备份，不算 B 自己的视频；
// C 从没见过 A，也能在 A 的名下看到这个视频，从 B 的备份播放原片，看到 A 的批注；C 再从 B 那里转手拿，备份仍然记在 A 名下。
func TestBackupPlaysWithoutOwner(t *testing.T) {
	old := retryBase
	retryBase = 10 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
	a, b, c := newTestApp(t, "A"), newTestApp(t, "B"), newTestApp(t, "C")
	content := strings.Repeat("frame-", 50000)
	src := writeShareFile(t, a, store.OutDirName+"/LAX43_EP03.mp4", content)
	past := time.Now().Add(-2 * time.Hour) // 修改时间在一分钟以前的文件扫描时直接算写完
	os.Chtimes(src, past, past)
	a.rescan()
	if len(a.listing()) != 1 {
		t.Fatalf("A 应该分享了一个视频：%+v", a.listing())
	}
	fp := a.listing()[0].FP
	body := `{"video":{"fp":"` + fp + `","name":"LAX43_EP03.mp4","owner":"` + a.me().ID + `","ownerName":"A","fps":25},"content":{"frame":10,"seconds":0.4,"text":"A 的批注"}}`
	if w := localPost(t, a, "/api/annotations", body); w.Code != http.StatusOK {
		t.Fatalf("写批注失败：%d %s", w.Code, w.Body.String())
	}

	srvA := httptest.NewServer(a.routes())
	defer srvA.Close()
	b.peers.seen(a.me().ID, "A", strings.TrimPrefix(srvA.URL, "http://"))
	b.syncPeer(b.peers.Snapshot()[0])
	if err := b.startDownloads(a.me().ID, []string{store.OutDirName + "/LAX43_EP03.mp4"}); err != nil {
		t.Fatal(err)
	}
	waitDownloads(t, b)
	local := filepath.Join(b.config().ShareDir, store.BackupDirName, "A", "LAX43_EP03.mp4")
	if got := readFile(t, local); got != content {
		t.Fatal("B 拿到的内容不对")
	}
	b.rescan()
	if e := findEntry(b.listing(), fp); e == nil || e.Origin != a.me().ID || e.OriginName != "A" {
		t.Fatalf("B 发给同事的列表里，这个视频应该记着原主人是 A：%+v", e)
	}
	mine, peers := b.videoView()
	if findEntry(mine, fp) != nil {
		t.Fatal("备份不应该算 B 自己的视频")
	}
	if e := findEntry(peers[0].Videos, fp); peers[0].ID != a.me().ID || e == nil || len(e.Holders) != 1 || e.Holders[0] != "本机" || !e.Local || e.Backup {
		t.Fatalf("B 的网页上，备份应该归在 A 名下、显示 A 的原版、标着本机有备份：%+v", e)
	}
	if vs := b.getVersions(); len(vs.Listing(b.videos.List(), nil)) != 0 {
		t.Fatal("备份不应该进 B 自己的版本组")
	}

	// A 改了视频名称，B 同步后备份里记的名称跟着变
	group := a.listing()[0].Group
	if w := localPost(t, a, "/api/versions/rename", `{"group":"`+group+`","name":"EP03 定剪"}`); w.Code != http.StatusOK {
		t.Fatalf("改名失败：%d %s", w.Code, w.Body.String())
	}
	b.syncPeer(b.peers.Snapshot()[0])
	if e := findEntry(b.listing(), fp); e == nil || e.Title != "EP03 定剪" {
		t.Fatalf("A 改名后，B 的备份应该跟着叫 EP03 定剪：%+v", e)
	}

	// C 只认识 B
	srvB := httptest.NewServer(b.routes())
	defer srvB.Close()
	srvA.Close() // A 不在了
	c.peers.seen(b.me().ID, "B", strings.TrimPrefix(srvB.URL, "http://"))
	c.syncPeer(c.peers.Snapshot()[0])
	_, peers = c.videoView()
	var groupA *Peer
	for i := range peers {
		if peers[i].ID == a.me().ID {
			groupA = &peers[i]
		} else if findEntry(peers[i].Videos, fp) != nil {
			t.Fatal("备份不应该出现在 B 的名下")
		}
	}
	if groupA == nil || groupA.Name != "A" || groupA.Online {
		t.Fatalf("C 没见过 A，也应该补一个离线的 A：%+v", peers)
	}
	if e := findEntry(groupA.Videos, fp); e == nil || len(e.Holders) != 1 || e.Holders[0] != "B" {
		t.Fatalf("C 看到的 A 的视频应该标着 B 有备份：%+v", groupA.Videos)
	}
	items, _ := c.getStore().Annotations(fp, "")
	if len(items) != 1 || items[0].Content.Text != "A 的批注" {
		t.Fatalf("C 应该从 B 那里同步到 A 的批注：%+v", items)
	}
	r := httptest.NewRequest("GET", "http://localhost:28380/api/stream/"+fp+"?owner="+a.me().ID, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	c.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != content {
		t.Fatalf("C 应该从 B 的备份播放原片：%d，%d 字节", w.Code, w.Body.Len())
	}

	// C 没见过 A，看到的是备份版
	if e := findEntry(groupA.Videos, fp); !e.Backup || e.Local {
		t.Fatalf("C 只有 B 的备份，应该显示备份版：%+v", e)
	}

	// C 点"备份视频"：A 不在，从 B 那里按指纹拿，存到自己的"备份/A/"，原主人仍然是 A
	if err := c.startVideoBackup(a.me().ID, fp); err != nil {
		t.Fatal(err)
	}
	if err := c.startVideoBackup(a.me().ID, fp); err == nil || !strings.Contains(err.Error(), "已经在备份") {
		t.Fatalf("同一个视频正在备份时不应该再建一项：%v", err)
	}
	if v := waitDownloads(t, c); v[0].State != "done" || v[0].FP != fp {
		t.Fatalf("应该备份完：%+v", v[0])
	}
	if got := readFile(t, filepath.Join(c.config().ShareDir, store.BackupDirName, "A", "LAX43_EP03.mp4")); got != content {
		t.Fatal("C 备份的视频应该存到 备份/A/，内容和原片一样")
	}
	c.rescan()
	if e := findEntry(c.listing(), fp); e == nil || e.Origin != a.me().ID {
		t.Fatalf("从 B 拿的备份原主人仍然是 A：%+v", e)
	}
	if _, peers := c.videoView(); findEntry(peers[len(peers)-1].Videos, fp) == nil || !findEntry(peers[len(peers)-1].Videos, fp).Local {
		t.Fatalf("C 备份完以后应该标着本机有备份：%+v", peers)
	}
	if err := c.startVideoBackup(a.me().ID, fp); err == nil {
		t.Fatal("本机已经有了，不应该再备份")
	}
}

// 原主人删了自己的文件（还有批注，列表里标"文件已不在"）：别人那里仍然在原主人名下，换成备份版，
// 不替原主人复原文件；原主人的文件还在时显示原版。
func TestBackupVersionWhenOwnerDeleted(t *testing.T) {
	old := retryBase
	retryBase = 10 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
	a, b := newTestApp(t, "A"), newTestApp(t, "B")
	src := writeShareFile(t, a, store.OutDirName+"/a.mp4", strings.Repeat("x", 10000))
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(src, past, past)
	a.rescan()
	fp := a.listing()[0].FP
	body := `{"video":{"fp":"` + fp + `","name":"a.mp4","owner":"` + a.me().ID + `","ownerName":"A","fps":25},"content":{"frame":1,"seconds":0.04,"text":"留着批注"}}`
	if w := localPost(t, a, "/api/annotations", body); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	b.peers.seen(a.me().ID, "A", strings.TrimPrefix(srv.URL, "http://"))
	b.syncPeer(b.peers.Snapshot()[0])
	if err := b.startVideoBackup(a.me().ID, fp); err != nil {
		t.Fatal(err)
	}
	waitDownloads(t, b)
	b.rescan()

	os.Remove(src)
	a.rescan()
	if e := findEntry(a.listing(), fp); e == nil || !e.Missing {
		t.Fatalf("A 删了文件，列表里应该标文件已不在：%+v", e)
	}
	b.syncPeer(b.peers.Snapshot()[0])
	_, peers := b.videoView()
	if e := findEntry(peers[0].Videos, fp); e == nil || !e.Backup || e.Missing || !e.Local {
		t.Fatalf("A 删了文件，B 这里应该在 A 名下显示备份版：%+v", e)
	}
	if _, err := os.Stat(src); err == nil {
		t.Fatal("不应该替 A 复原文件")
	}
}

func TestBackupPath(t *testing.T) {
	root := filepath.Join("共享", store.BackupDirName)
	for rel, want := range map[string]string{
		store.OutDirName + "/素材/a.mov":     filepath.Join(root, "小A", "素材", "a.mov"),
		store.OutDirName:                   filepath.Join(root, "小A"),
		"旧版本放在根目录的.mov":                    filepath.Join(root, "小A", "旧版本放在根目录的.mov"),
		store.BackupDirName + "/小C/b?.mov": filepath.Join(root, "小C", "b_.mov"),
		store.BackupDirName + "/小C":        filepath.Join(root, "小C"),
		store.BackupDirName:                root,
	} {
		if got := backupPath(root, "小A", rel); got != want {
			t.Errorf("backupPath(%q) = %q，应该是 %q", rel, got, want)
		}
	}
}

// 同事的列表 JSON 里带着原主人，0.3.x 不认这两个字段也能照常解析。
func TestBackupEntryJSON(t *testing.T) {
	data, _ := json.Marshal(&media.VideoEntry{VideoInfo: media.VideoInfo{FP: "f"}, Origin: "o", OriginName: "小A"})
	if !strings.Contains(string(data), `"origin":"o"`) || strings.Contains(string(data), "holders") {
		t.Fatalf("列表项 JSON 不对：%s", data)
	}
}
