package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"lanreview/internal/store"
)

// newTestApp 是一个设置好昵称和共享文件夹的实例，不启动发现和同步的后台循环，测试里手动调用。
func newTestApp(t *testing.T, name string) *App {
	a, err := newApp(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.setup(name, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	return a
}

// A 在网页上写的批注，B 通过 A 的 /peer/ 接口同步一轮后就有了。
func TestSyncBetweenApps(t *testing.T) {
	a, b := newTestApp(t, "A"), newTestApp(t, "B")
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	body := `{"video":{"fp":"0123456789abcdef0123456789abcdef","name":"x.mp4","owner":"` + a.me().ID + `","ownerName":"A","fps":25},
		"content":{"frame":10,"seconds":0.4,"text":"测试批注"}}`
	r := httptest.NewRequest(http.MethodPost, "http://localhost:28380/api/annotations", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("写批注失败：%d %s", w.Code, w.Body.String())
	}
	var created struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &created)

	addr := strings.TrimPrefix(srv.URL, "http://")
	b.peers.seen(a.me().ID, "A", addr)
	b.syncPeer(Peer{ID: a.me().ID, Addr: addr})
	if !b.getStore().Has(created.ID) {
		t.Fatal("B 没有同步到 A 的批注")
	}
}

// 对方的记录校验不通过时，下一轮同步不再重新下载。
func TestRejectedRecordNotRefetched(t *testing.T) {
	b := newTestApp(t, "B")
	const peerID = "fake-peer"
	recID := strings.Repeat("ab", 16)
	var fetches atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /peer/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, peerInfo{App: appName, ID: peerID, Name: "坏记录"})
	})
	mux.HandleFunc("GET /peer/videos", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, []any{}) })
	mux.HandleFunc("GET /peer/records", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []store.RecordStub{{ID: recID, Ann: recID, Type: "create"}})
	})
	mux.HandleFunc("GET /peer/record/{id}", func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		writeJSON(w, store.Record{ID: recID, Ann: recID, Type: "create"}) // 没有内容也没有签名
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := Peer{ID: peerID, Addr: strings.TrimPrefix(srv.URL, "http://")}
	b.peers.seen(p.ID, "坏记录", p.Addr)
	for range 3 {
		b.syncPeer(p)
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("被拒绝的记录下载了 %d 次，应该只下载 1 次", n)
	}
	if b.getStore().Has(recID) {
		t.Fatal("不应该收下校验不通过的记录")
	}
}

// 成员目录只记口令一致的同事；没确认过的旧记录不留；离线的人能移除，再上线时自动回来。
func TestPeopleDirectory(t *testing.T) {
	dataDir := t.TempDir()
	// 以前留下的、没确认过是同一个团队的记录（比如同一个局域网里开着的测试程序）
	os.WriteFile(filepath.Join(dataDir, "people.json"), []byte(`{"stale":{"name":"测试C","lastSeen":1}}`), 0o644)
	a, err := newApp(dataDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.setup("A", t.TempDir(), "口令"); err != nil {
		t.Fatal(err)
	}
	peer := func(name, key string) (*App, string) {
		p, err := newApp(t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.setup(name, t.TempDir(), key); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(p.routes())
		t.Cleanup(srv.Close)
		addr := strings.TrimPrefix(srv.URL, "http://")
		a.peers.seen(p.me().ID, name, addr)
		a.syncPeer(Peer{ID: p.me().ID, Addr: addr})
		return p, addr
	}
	b, bAddr := peer("B", "口令")
	c, _ := peer("C", "别的口令")
	names := func() map[string]bool {
		_, peers := a.videoView()
		out := map[string]bool{}
		for _, p := range a.people(peers) {
			out[p.Name] = true
		}
		return out
	}

	a.peers.saveKnown(dataDir)
	var saved map[string]knownPeer
	data, _ := os.ReadFile(filepath.Join(dataDir, "people.json"))
	json.Unmarshal(data, &saved)
	if !saved[b.me().ID].Team || len(saved) != 1 {
		t.Fatalf("people.json 应该只记口令一致的 B：%s", data)
	}
	if _, ok := saved[c.me().ID]; ok {
		t.Fatal("口令不一致的 C 不应该记下来")
	}
	if names()["测试C"] {
		t.Fatal("没确认过的旧记录不应该出现在成员列表里")
	}

	if err := a.removePerson(b.me().ID); err == nil {
		t.Fatal("B 在线，不应该能移除")
	}
	a.peers.update(b.me().ID, func(p *Peer) { p.LastSeen = 0 }) // B 下线了
	if err := a.removePerson(b.me().ID); err != nil {
		t.Fatal(err)
	}
	if names()["B"] {
		t.Fatal("移除后 B 不应该在成员列表里")
	}
	// 重启后仍然是移除的
	a.peers = newPeers()
	a.peers.loadKnown(dataDir)
	if names()["B"] {
		t.Fatal("重启后 B 仍然应该是移除的")
	}
	// B 再上线：自动回到列表里
	a.peers.seen(b.me().ID, "B", bAddr)
	a.syncPeer(Peer{ID: b.me().ID, Addr: bAddr})
	a.peers.saveKnown(dataDir)
	if !names()["B"] {
		t.Fatal("B 再上线后应该回到成员列表里")
	}
}
