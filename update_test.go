package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"lanreview/internal/update"
)

// releaseTo 给 a 的更新缓存放进一个签好名的新版本，本机系统和另一个系统各一个安装包，返回安装包内容。
func releaseTo(t *testing.T, a *App, priv ed25519.PrivateKey, ver string) map[string][]byte {
	pkgs := map[string][]byte{runtime.GOOS: []byte("本机系统的新版本 " + ver), "plan9": []byte("另一个系统的新版本 " + ver)}
	m := update.Manifest{Version: ver, Files: map[string]update.File{}}
	for goos, data := range pkgs {
		sum := sha256.Sum256(data)
		m.Files[goos] = update.File{Name: "lan-video-review-" + goos + ".zip", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	}
	raw, err := update.Sign(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	pm, err := update.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.upd.cache.Put(raw, pm); err != nil {
		t.Fatal(err)
	}
	for goos, data := range pkgs {
		path, _, _ := a.upd.cache.Path(goos)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return pkgs
}

// 同事 A 已经有新版本，B 同步时看到后从 A 那里拿到清单和两个系统的安装包，可以更新，也能再转给别人。
func TestUpdateFromPeer(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	t.Cleanup(update.SetPublicKey(pub))
	old := version
	version = "0.3.0"
	t.Cleanup(func() { version = old })
	t.Setenv("LANFRAME_UPDATE_BASE", "http://127.0.0.1:1") // 不让测试连真的 GitHub

	a, b := newTestApp(t, "A"), newTestApp(t, "B")
	pkgs := releaseTo(t, a, priv, "0.3.1")
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	var info peerInfo
	if err := b.getJSON(context.Background(), "http://"+addr+"/peer/info", &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "0.3.0" || info.Update != "0.3.1" {
		t.Fatalf("/peer/info 里的版本是 %q、%q", info.Version, info.Update)
	}

	b.peers.seen(a.me().ID, "A", addr)
	b.syncPeer(b.peers.Snapshot()[0])
	if p := b.peers.Snapshot()[0]; p.Update != "0.3.1" {
		t.Fatalf("同步后应该记下 A 缓存的版本，现在是 %q", p.Update)
	}
	b.checkUpdate(updateKick{})
	st := b.updateState()
	if !st.Ready || st.Latest != "0.3.1" {
		t.Fatalf("B 应该已经下载好 0.3.1：%+v", st)
	}
	for goos, data := range pkgs {
		path, _, ok := b.upd.cache.Path(goos)
		got, _ := os.ReadFile(path)
		if !ok || string(got) != string(data) {
			t.Errorf("B 缓存里 %s 的安装包不对", goos)
		}
	}
	// B 现在也能把新版本给别人
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://x/peer/update", nil)
	r.RemoteAddr = "192.168.1.30:50000"
	b.routes().ServeHTTP(w, r)
	if m, err := update.Parse(w.Body.Bytes()); err != nil || m.Version != "0.3.1" {
		t.Fatalf("B 的 /peer/update 应该返回 0.3.1 的清单：%v", err)
	}
}

// 同事给的安装包和签名的清单对不上（被改过），不能用；伪造签名的清单直接不理。
func TestUpdateFromBadPeer(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	t.Cleanup(update.SetPublicKey(pub))
	old := version
	version = "0.3.0"
	t.Cleanup(func() { version = old })
	t.Setenv("LANFRAME_UPDATE_BASE", "http://127.0.0.1:1")

	a, b := newTestApp(t, "A"), newTestApp(t, "B")
	releaseTo(t, a, priv, "0.3.1")
	path, _, _ := a.upd.cache.Path(runtime.GOOS)
	data, _ := os.ReadFile(path)
	data[0] ^= 1
	os.WriteFile(path, data, 0o644)
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	b.peers.seen(a.me().ID, "A", srv.Listener.Addr().String())
	b.syncPeer(b.peers.Snapshot()[0])
	b.checkUpdate(updateKick{})
	if st := b.updateState(); st.Ready || st.Error == "" {
		t.Fatalf("被改过的安装包不应该能用，并且要说明原因：%+v", st)
	}

	// 换一个伪造的 0.3.2：别的私钥签的
	c := newTestApp(t, "C")
	_, fake, _ := ed25519.GenerateKey(rand.Reader)
	restore := update.SetPublicKey(fake.Public().(ed25519.PublicKey))
	releaseTo(t, c, fake, "0.3.2")
	restore()
	srv2 := httptest.NewServer(c.routes())
	defer srv2.Close()
	b.peers.seen(c.me().ID, "C", srv2.Listener.Addr().String())
	for _, p := range b.peers.Snapshot() {
		if p.ID == c.me().ID {
			b.syncPeer(p)
		}
	}
	b.checkUpdate(updateKick{manual: true})
	if v := b.upd.cache.Version(); v != "0.3.1" {
		t.Fatalf("伪造的版本不应该进缓存，现在缓存的是 %s", v)
	}
}

// 没有缓存新版本时 /peer/update 返回 404；浏览器访问不了。
func TestPeerUpdateGuards(t *testing.T) {
	a := newTestApp(t, "A")
	h := a.routes()
	for _, c := range []struct {
		path, fetchSite string
		want            int
	}{
		{"/peer/update", "", 404},
		{"/peer/update/lan-video-review-mac.zip", "", 404},
		{"/peer/update", "cross-site", 403},
	} {
		r := httptest.NewRequest("GET", "http://x"+c.path, nil)
		r.RemoteAddr = "192.168.1.20:50000"
		if c.fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", c.fetchSite)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s 返回 %d，应该是 %d", c.path, w.Code, c.want)
		}
	}
	// 下载页优先给缓存里的安装包
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	t.Cleanup(update.SetPublicKey(pub))
	releaseTo(t, a, priv, "0.3.1")
	files := a.downloadFiles()
	if len(files) != 2 || filepath.Dir(files[0].Path) != filepath.Join(a.dataDir, "updates", "0.3.1") {
		t.Fatalf("下载页的安装包不对：%+v", files)
	}
	var st map[string]json.RawMessage
	r := httptest.NewRequest("GET", "http://localhost:28380/api/state", nil)
	r.RemoteAddr = "127.0.0.1:50000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st["update"] == nil {
		t.Fatalf("/api/state 里应该有 update：%s", w.Body.String())
	}
}
