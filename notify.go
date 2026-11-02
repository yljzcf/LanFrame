package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lanreview/internal/store"
)

// 提醒：和我有关的批注、回复、@，从批注记录算出来（internal/store/inbox.go）。
// 已读状态只记在本机配置目录的 inbox.json 里，不同步给别的电脑。

const inboxLimit = 100 // 提醒列表最多给网页这么多条

type inboxState struct {
	ReadBefore int64           `json:"readBefore,omitempty"` // 点过"全部已读"：这个时间以前的都算已读
	Read       map[string]bool `json:"read,omitempty"`       // 单独看过的提醒（记录 ID）
}

type inbox struct {
	path string

	mu     sync.Mutex
	st     inboxState
	cache  []store.InboxItem // 按批注记录算出来的提醒，记录有变化时重算
	cached int64             // 算 cache 时批注记录的版本
	store  *store.Store
	unread int // 上次告诉托盘的未读数
}

func loadInbox(dataDir string) *inbox {
	in := &inbox{path: filepath.Join(dataDir, "inbox.json"), cached: -1}
	if data, err := os.ReadFile(in.path); err == nil {
		if err := json.Unmarshal(data, &in.st); err != nil {
			log.Printf("提醒的已读记录坏了，从头记：%v", err)
		}
	}
	if in.st.Read == nil {
		in.st.Read = map[string]bool{}
	}
	return in
}

// itemsLocked 是当前的提醒，带上已读标记。
func (in *inbox) itemsLocked(st *store.Store, me string) []store.InboxItem {
	if st == nil {
		return nil
	}
	if v := st.Version(); st != in.store || v != in.cached {
		in.cache, in.store, in.cached = st.Inbox(me), st, v
	}
	out := make([]store.InboxItem, len(in.cache))
	for i, it := range in.cache {
		it.Read = it.Time <= in.st.ReadBefore || in.st.Read[it.ID]
		out[i] = it
	}
	return out
}

func (in *inbox) saveLocked() {
	data, _ := json.MarshalIndent(in.st, "", "  ")
	if err := store.WriteFileAtomic(in.path, data); err != nil {
		log.Printf("保存提醒的已读记录失败：%v", err)
	}
}

func (a *App) inboxItems() []store.InboxItem {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return a.inbox.itemsLocked(a.getStore(), a.me().ID)
}

// inboxSummary 是 /api/state 里的提醒概况：未读数，和最新一条的 ID（变了说明有新提醒，网页再来取列表）。
func (a *App) inboxSummary() (unread int, newest string) {
	items := a.inboxItems()
	for _, it := range items {
		if !it.Read {
			unread++
		}
	}
	if len(items) > 0 {
		newest = items[0].ID
	}
	return unread, newest
}

// checkInbox 定时看看未读数变了没有，变了就刷新托盘。
func (a *App) checkInbox() {
	n, _ := a.inboxSummary()
	a.inbox.mu.Lock()
	changed := n != a.inbox.unread
	a.inbox.unread = n
	a.inbox.mu.Unlock()
	if changed {
		a.changed()
	}
}

func (a *App) apiInbox(w http.ResponseWriter, r *http.Request) {
	items := a.inboxItems()
	if len(items) > inboxLimit {
		items = items[:inboxLimit]
	}
	writeJSON(w, map[string]any{"items": items})
}

// apiInboxRead 记已读：ids 是点过的提醒，fp 是打开过的视频（这个视频的提醒都算看过），all 是"全部已读"。
func (a *App) apiInboxRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string
		FP  string
		All bool
	}
	if err := readJSON(r, &body); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	in := a.inbox
	in.mu.Lock()
	items := in.itemsLocked(a.getStore(), a.me().ID)
	ids := map[string]bool{}
	for _, id := range body.IDs {
		ids[id] = true
	}
	changed := false
	if body.All {
		// 提醒的时间是写批注那台电脑的时钟，可能比本机快，取两者里大的
		in.st.ReadBefore = time.Now().UnixMilli()
		if len(items) > 0 && items[0].Time > in.st.ReadBefore {
			in.st.ReadBefore = items[0].Time
		}
		changed = true
	}
	for _, it := range items {
		if !it.Read && !body.All && (ids[it.ID] || (body.FP != "" && it.Video.FP == body.FP)) {
			in.st.Read[it.ID] = true
			changed = true
		}
	}
	if changed {
		// 只留还在列表里、又没被"全部已读"盖住的，文件不会越记越大
		live := map[string]bool{}
		for _, it := range items {
			if in.st.Read[it.ID] && it.Time > in.st.ReadBefore {
				live[it.ID] = true
			}
		}
		in.st.Read = live
		in.saveLocked()
	}
	in.mu.Unlock()
	if changed {
		a.checkInbox()
	}
	writeJSON(w, map[string]bool{"ok": true})
}
