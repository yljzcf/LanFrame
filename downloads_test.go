package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lanreview/internal/store"
)

// downloadPair 是 A（开放文件）和 B（来拿文件）。B 已经发现了 A，下载存到一个临时文件夹。
func downloadPair(t *testing.T, handler func(a *App) http.Handler) (a, b *App, dest string) {
	t.Helper()
	old := retryBase
	retryBase = 10 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
	a, b = newTestApp(t, "A"), newTestApp(t, "B")
	h := a.routes()
	if handler != nil {
		h = handler(a)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	b.peers.seen(a.me().ID, "A", strings.TrimPrefix(srv.URL, "http://"))
	b.peers.update(a.me().ID, func(p *Peer) { p.Files = true })
	dest = filepath.Join(b.config().ShareDir, store.BackupDirName, "A")
	return a, b, dest
}

// waitDownloads 不停地安排下载，直到所有下载都结束，返回下载列表。
func waitDownloads(t *testing.T, a *App) []dlView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a.scheduleDownloads()
		a.dl.mu.Lock()
		all := true
		for _, j := range a.dl.jobs {
			if !j.finished() || hasActive(j) {
				all = false
			}
		}
		a.dl.mu.Unlock()
		if all {
			return a.downloadsView()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("10 秒内没下载完：%+v", a.downloadsView())
	return nil
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// 整个文件夹拿到"备份/A/"里：子文件夹、中文名照原样；Windows 不认的文件名改掉；隐藏文件不拿；
// 修改时间和对方一致。再拿一次，已经有的一样的文件不再下；对方改过的另存成"名字 (1)"，不覆盖。
func TestDownloadFolder(t *testing.T) {
	a, b, dest := downloadPair(t, nil)
	src := writeShareFile(t, a, "素材/镜头1.mov", strings.Repeat("镜头", 1000))
	old := time.Date(2026, 3, 1, 10, 0, 0, 123e6, time.Local)
	os.Chtimes(src, old, old)
	writeShareFile(t, a, "素材/子/说明?.txt", "readme")
	writeShareFile(t, a, "素材/CON.txt", "con")
	writeShareFile(t, a, "素材/.DS_Store", "x")
	writeShareFile(t, a, "单个.txt", "one")

	if err := b.startDownloads(a.me().ID, []string{"素材", "单个.txt"}); err != nil {
		t.Fatal(err)
	}
	views := waitDownloads(t, b)
	if len(views) != 2 || views[0].State != "done" || views[1].State != "done" || views[1].Files != 3 {
		t.Fatalf("两项都应该下载完，文件夹里 3 个文件：%+v", views)
	}
	if got := readFile(t, filepath.Join(dest, "素材", "镜头1.mov")); got != strings.Repeat("镜头", 1000) {
		t.Fatal("镜头1.mov 内容不对")
	}
	st, _ := os.Stat(filepath.Join(dest, "素材", "镜头1.mov"))
	if st.ModTime().UnixMilli() != old.UnixMilli() {
		t.Errorf("修改时间应该和对方一致：%v，应该是 %v", st.ModTime(), old)
	}
	if got := readFile(t, filepath.Join(dest, "素材", "子", "说明_.txt")); got != "readme" {
		t.Errorf("说明?.txt 应该改名成 说明_.txt：%q", got)
	}
	if got := readFile(t, filepath.Join(dest, "素材", "_CON.txt")); got != "con" {
		t.Errorf("CON.txt 应该改名成 _CON.txt：%q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "素材", ".DS_Store")); err == nil {
		t.Error("隐藏文件不应该下载")
	}
	if got := readFile(t, filepath.Join(dest, "单个.txt")); got != "one" {
		t.Errorf("单个文件内容不对：%q", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dest, "*", "*"+partExt))
	if len(matches) > 0 {
		t.Errorf("下载完不应该留下 %s：%v", partExt, matches)
	}

	one := filepath.Join(a.config().ShareDir, "单个.txt")
	os.WriteFile(one, []byte("改过的"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(one, later, later)
	if err := b.startDownloads(a.me().ID, []string{"素材", "单个.txt"}); err != nil {
		t.Fatal(err)
	}
	if v := b.downloadsView(); v[1].State != "done" || v[1].Name != "素材" {
		t.Errorf("素材已经备份过了，应该直接算下完：%+v", v[1])
	}
	waitDownloads(t, b)
	if _, err := os.Stat(filepath.Join(dest, "素材 (1)")); err == nil {
		t.Error("再拿一次文件夹应该合并进原来的，不另建 素材 (1)")
	}
	if got := readFile(t, filepath.Join(dest, "单个 (1).txt")); got != "改过的" {
		t.Errorf("对方改过的应该另存成 单个 (1).txt：%q", got)
	}
	if got := readFile(t, filepath.Join(dest, "单个.txt")); got != "one" {
		t.Errorf("原来的 单个.txt 不应该被覆盖：%q", got)
	}
}

// 下到一半连接断了，接着从断点下，最后内容逐字节一致。
func TestDownloadResume(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 100000) // 1MB
	var mu sync.Mutex
	var ranges []string
	_, b, dest := downloadPair(t, func(a *App) http.Handler {
		writeShareFile(t, a, "大文件.mp4", string(content))
		h := a.routes()
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/peer/file" {
				h.ServeHTTP(w, r)
				return
			}
			mu.Lock()
			ranges = append(ranges, r.Header.Get("Range"))
			first := len(ranges) == 1
			mu.Unlock()
			if !first {
				h.ServeHTTP(w, r)
				return
			}
			// 第一次只给一半就断开
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes()[:len(content)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		})
	})
	a := b.peers.Snapshot()[0]
	if err := b.startDownloads(a.ID, []string{"大文件.mp4"}); err != nil {
		t.Fatal(err)
	}
	views := waitDownloads(t, b)
	if views[0].State != "done" {
		t.Fatalf("应该下载完：%+v", views[0])
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "大文件.mp4")); !bytes.Equal(got, content) {
		t.Fatal("续传后的内容和原文件不一致")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 2 || ranges[0] != "" || ranges[1] != "bytes=500000-" {
		t.Fatalf("第二次应该从断点 500000 接着下：%q", ranges)
	}
}

// 已经下了一部分，对方的文件改过了（修改时间对不上），从头下新的内容。
func TestDownloadSourceChanged(t *testing.T) {
	a, b, dest := downloadPair(t, nil)
	src := writeShareFile(t, a, "a.txt", "旧的内容，很长很长很长")
	if err := b.startDownloads(a.me().ID, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dest, "a.txt"+partExt), []byte("旧的"), 0o644)
	os.WriteFile(src, []byte("新内容"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(src, later, later)

	views := waitDownloads(t, b)
	if views[0].State != "done" || views[0].Size != int64(len("新内容")) {
		t.Fatalf("应该按新的大小下载完：%+v", views[0])
	}
	if got := readFile(t, filepath.Join(dest, "a.txt")); got != "新内容" {
		t.Fatalf("应该是新的内容：%q", got)
	}
}

// 程序重启（比如自动更新）后，没下完的接着下，从已经下好的部分续上。
func TestDownloadAfterRestart(t *testing.T) {
	content := strings.Repeat("abcdefghij", 1000)
	var mu sync.Mutex
	var ranges []string
	a, b, dest := downloadPair(t, nil)
	writeShareFile(t, a, "素材/x.mov", content)
	if err := b.startDownloads(a.me().ID, []string{"素材"}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dest, "素材"), 0o755)
	os.WriteFile(filepath.Join(dest, "素材", "x.mov"+partExt), []byte(content[:4000]), 0o644)

	h := a.routes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peer/file" {
			mu.Lock()
			ranges = append(ranges, r.Header.Get("Range"))
			mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	b2, err := newApp(b.dataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	b2.peers.seen(a.me().ID, "A", strings.TrimPrefix(srv.URL, "http://"))
	if v := b2.downloadsView(); len(v) != 1 || v[0].Got != 4000 {
		t.Fatalf("重启后应该记得这项下载和已经下好的 4000 字节：%+v", v)
	}
	if v := waitDownloads(t, b2); v[0].State != "done" {
		t.Fatalf("应该下载完：%+v", v[0])
	}
	if got := readFile(t, filepath.Join(dest, "素材", "x.mov")); got != content {
		t.Fatal("内容不对")
	}
	if len(ranges) != 1 || ranges[0] != "bytes=4000-" {
		t.Fatalf("应该从 4000 字节接着下：%q", ranges)
	}
}

// 对方删了其中一个文件：这个文件记为失败，其余照常下完；取消时删掉没下完的部分。
func TestDownloadGoneAndCancel(t *testing.T) {
	a, b, dest := downloadPair(t, nil)
	writeShareFile(t, a, "素材/a.txt", "a")
	gone := writeShareFile(t, a, "素材/b.txt", "b")
	if err := b.startDownloads(a.me().ID, []string{"素材"}); err != nil {
		t.Fatal(err)
	}
	os.Remove(gone)
	v := waitDownloads(t, b)[0]
	if v.State != "partial" || v.Done != 1 || len(v.Failed) != 1 || !strings.Contains(v.Failed[0], "对方已经没有这个文件了") {
		t.Fatalf("应该是部分下载完，b.txt 失败：%+v", v)
	}

	writeShareFile(t, a, "c.txt", "c")
	if err := b.startDownloads(a.me().ID, []string{"c.txt"}); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dest, "c.txt"+partExt)
	os.WriteFile(part, []byte("半"), 0o644)
	id := b.downloadsView()[0].ID
	if err := b.cancelDownload(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); err == nil {
		t.Error("取消后应该删掉没下完的部分")
	}
	if v := b.downloadsView()[0]; v.State != "canceled" {
		t.Fatalf("应该是已取消：%+v", v)
	}
	b.clearDownloads()
	if v := b.downloadsView(); len(v) != 0 {
		t.Fatalf("清除后列表应该是空的：%+v", v)
	}
}

// 团队口令不一致时拿不到对方的文件列表，提示说清楚原因。
func TestDownloadTeamKeyMismatch(t *testing.T) {
	a, b, _ := downloadPair(t, nil)
	writeShareFile(t, a, "a.txt", "a")
	c := a.config()
	c.TeamKey = "口令"
	a.saveConfig(c)
	err := b.startDownloads(a.me().ID, []string{"a.txt"})
	if err == nil || !strings.Contains(err.Error(), "团队口令") {
		t.Fatalf("应该提示团队口令不一致：%v", err)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"镜头1.mov":     "镜头1.mov",
		`a?b*c:"d".x`: "a_b_c__d_.x",
		"con.txt":     "_con.txt",
		"LPT1":        "_LPT1",
		"结尾. ":        "结尾",
		"...":         "_",
		"COM10.txt":   "COM10.txt",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q，应该是 %q", in, got, want)
		}
	}
}
