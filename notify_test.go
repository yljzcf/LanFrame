package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lanreview/internal/store"
)

// 已读：点过的、打开过的视频、全部已读；重启以后还记得。
func TestInboxRead(t *testing.T) {
	dataDir := t.TempDir()
	a, err := newApp(dataDir, 28380)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.openShare(t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	other := mustIdentity(t)
	st := a.getStore()
	comment := func(fp string) string {
		now := time.Now().UnixMilli() + st.NextClock() // 每条都比上一条晚
		ann := store.AuthorTag(other.ID) + store.RandomHex(8)
		r := &store.Record{ID: ann, Type: "create", Ann: ann, Clock: st.NextClock(), Time: now,
			Author: other.ID, AuthorName: "B", Video: &store.VideoRef{FP: fp, Owner: a.me().ID},
			Content: &store.Content{Text: "批注"}}
		if err := other.Sign(r); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Add(r); err != nil {
			t.Fatal(err)
		}
		return ann
	}
	read := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		a.apiInboxRead(w, httptest.NewRequest("POST", "/api/inbox/read", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("记已读失败：%d %s", w.Code, w.Body.String())
		}
	}
	unread := func() int { n, _ := a.inboxSummary(); return n }

	first := comment("f1")
	comment("f1")
	comment("f2")
	if n := unread(); n != 3 {
		t.Fatalf("应该有 3 条未读，实际 %d", n)
	}
	read(`{"ids":["` + first + `"]}`)
	if n := unread(); n != 2 {
		t.Fatalf("点过一条以后应该剩 2 条，实际 %d", n)
	}
	read(`{"fp":"f1"}`)
	if n := unread(); n != 1 {
		t.Fatalf("打开过 f1 以后应该剩 1 条，实际 %d", n)
	}
	read(`{"all":true}`)
	if n := unread(); n != 0 {
		t.Fatalf("全部已读以后应该是 0，实际 %d", n)
	}
	// 重启后已读状态还在，新来的批注照样是未读
	a.inbox = loadInbox(dataDir)
	comment("f2")
	if n := unread(); n != 1 {
		t.Fatalf("重启后应该只有新来的 1 条未读，实际 %d", n)
	}
}
