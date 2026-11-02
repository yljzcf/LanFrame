package store

import (
	"slices"
	"sort"
	"strings"
)

// InboxItem 是一条和"我"有关的批注或回复：我的视频收到的、@ 我的、回复我的。
// 从批注记录直接算出来，不另外同步；已读状态由调用方另外记。
type InboxItem struct {
	ID         string   `json:"id"`   // 记录 ID：批注、回复是 create 记录；修改时新 @ 了我是那条 edit 记录
	Ann        string   `json:"ann"`  // 批注 ID
	Root       string   `json:"root"` // 原批注 ID：回复是它挂着的那条，批注是它自己
	Kind       string   `json:"kind"` // mention | reply | comment
	Time       int64    `json:"time"`
	Author     string   `json:"author"`
	AuthorName string   `json:"authorName"`
	Video      VideoRef `json:"video"`
	Frame      int      `json:"frame"`
	Text       string   `json:"text"`
	IsReply    bool     `json:"isReply"`
	Read       bool     `json:"read"`
}

// Inbox 算出和 me 有关的提醒，新的在前。规则：
//   - mention：@ 了我（修改时新加上的 @ 也算，时间用修改的时间）；
//   - reply：回复了我，或者回复了我写的批注、我回复过的讨论；
//   - comment：在我的视频上写的批注和回复。
//
// 自己写的、已经删除的不算；一条记录只出一条提醒，按 mention、reply、comment 的顺序取第一个符合的。
func (s *Store) Inbox(me string) []InboxItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	creates := map[string]*Record{}
	edits := map[string][]*Record{}
	threads := map[string][]*Record{} // 原批注 ID -> 这条讨论里的批注和回复
	for _, r := range s.recs {
		if s.deleted[r.Ann] != nil {
			continue
		}
		switch r.Type {
		case "create":
			creates[r.ID] = r
			root := r.ID
			if r.ReplyTo != "" {
				root = r.ReplyTo
			}
			threads[root] = append(threads[root], r)
		case "edit":
			edits[r.Ann] = append(edits[r.Ann], r)
		}
	}
	// 我在这条讨论里说过话：原批注是我写的，或者在 t 之前回复过
	spoke := func(root string, t int64) bool {
		for _, r := range threads[root] {
			if r.Author == me && (r.ID == root || r.Time < t) {
				return true
			}
		}
		return false
	}
	items := []InboxItem{}
	for ann, c := range creates {
		if c.Author == me {
			continue
		}
		latest := *c.Content
		es := edits[ann]
		sortRecords(es)
		for _, e := range es {
			if e.Author == c.Author {
				latest = *e.Content
			}
		}
		root := ann
		if c.ReplyTo != "" {
			root = c.ReplyTo
		}
		item := func(r *Record, kind string) InboxItem {
			return InboxItem{
				ID: r.ID, Ann: ann, Root: root, Kind: kind, Time: r.Time,
				Author: c.Author, AuthorName: c.AuthorName, Video: *c.Video,
				Frame: c.Content.Frame, Text: summary(latest), IsReply: c.ReplyTo != "",
			}
		}
		kind := ""
		switch {
		case slices.Contains(c.Content.Mentions, me):
			kind = "mention"
		case c.ReplyTo != "" && ((c.Quote != nil && c.Quote.Author == me) || spoke(root, c.Time)):
			kind = "reply"
		case c.Video.Owner == me:
			kind = "comment"
		}
		if kind != "" {
			items = append(items, item(c, kind))
		}
		// 修改时新 @ 了我
		prev := c.Content.Mentions
		for _, e := range es {
			if e.Author != c.Author {
				continue
			}
			if slices.Contains(e.Content.Mentions, me) && !slices.Contains(prev, me) {
				items = append(items, item(e, "mention"))
			}
			prev = e.Content.Mentions
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Time != items[j].Time {
			return items[i].Time > items[j].Time
		}
		return items[i].ID > items[j].ID
	})
	return items
}

// summary 是提醒里显示的一小段内容。
func summary(c Content) string {
	text := strings.Join(strings.Fields(c.Text), " ")
	if r := []rune(text); len(r) > 120 {
		text = string(r[:120]) + "…"
	}
	switch {
	case text != "":
		return text
	case len(c.Attachments) > 0:
		return "[图片]"
	default:
		return "（画面标注）"
	}
}

// Person 是批注记录里出现过的人：写批注的人，和被批注视频的主人。
type Person struct {
	Name string
	Time int64 // 记录里这个名字最新的时间，用来和别处得到的名字比新旧
}

// People 是记录里出现过的人和他们最新的昵称。视频主人的昵称是写批注的人那时看到的，可能过时，
// 只在这个人自己没写过批注时才用。
func (s *Store) People() map[string]Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	authors, owners := map[string]Person{}, map[string]Person{}
	note := func(m map[string]Person, id, name string, t int64) {
		if id == "" || name == "" {
			return
		}
		if p, ok := m[id]; !ok || t > p.Time {
			m[id] = Person{name, t}
		}
	}
	for _, r := range s.recs {
		note(authors, r.Author, r.AuthorName, r.Time)
		if r.Video != nil {
			note(owners, r.Video.Owner, r.Video.OwnerName, r.Time)
		}
	}
	for id, p := range owners {
		if _, ok := authors[id]; !ok {
			authors[id] = p
		}
	}
	return authors
}
