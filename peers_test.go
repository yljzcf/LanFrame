package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
