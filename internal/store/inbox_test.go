package store

import (
	"testing"
)

// 三个人：A 是视频主人，B、C 是同事。
func TestInbox(t *testing.T) {
	a, b, c := newTestIdentity(t), newTestIdentity(t), newTestIdentity(t)
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	video := &VideoRef{FP: "f", Owner: a.ID, OwnerName: "A"}
	clock := int64(0)
	add := func(id *Identity, r *Record) *Record {
		t.Helper()
		clock++
		r.Clock, r.Time = clock, clock*1000
		if r.Type == "create" {
			r.Ann = AuthorTag(id.ID) + RandomHex(8)
			r.ID = r.Ann
		} else {
			r.ID = RandomHex(16)
		}
		if r.Video == nil {
			r.Video = video
		}
		signed(t, id, r)
		if ok, err := st.Add(r); !ok || err != nil {
			t.Fatalf("添加记录失败：%v", err)
		}
		return r
	}
	kinds := func(me string) map[string]string {
		out := map[string]string{}
		for _, it := range st.Inbox(me) {
			out[it.ID] = it.Kind
		}
		return out
	}

	byB := add(b, &Record{Type: "create", AuthorName: "B", Content: &Content{Text: "B 的批注"}})
	byA := add(a, &Record{Type: "create", AuthorName: "A", Content: &Content{Text: "A 自己的批注"}})
	mentionC := add(b, &Record{Type: "create", AuthorName: "B", Content: &Content{Text: "@C 看一下", Mentions: []string{c.ID}}})
	// C 回复 B 的批注：B 收到"回复"，A 是视频主人收到"批注"
	replyC := add(c, &Record{Type: "create", AuthorName: "C", ReplyTo: byB.ID, Quote: &Quote{Author: b.ID, Text: "B 的批注"}, Content: &Content{Text: "同意"}})
	// B 再回复：C 之前在这条讨论里说过话，收到"回复"
	replyB := add(b, &Record{Type: "create", AuthorName: "B", ReplyTo: byB.ID, Quote: &Quote{Author: c.ID, Text: "同意"}, Content: &Content{Text: "好的"}})

	ka := kinds(a.ID)
	if ka[byB.ID] != "comment" || ka[mentionC.ID] != "comment" || ka[replyC.ID] != "comment" || ka[replyB.ID] != "comment" {
		t.Fatalf("视频主人的提醒不对：%v", ka)
	}
	if _, ok := ka[byA.ID]; ok {
		t.Fatal("自己写的不应该提醒")
	}
	kb := kinds(b.ID)
	if len(kb) != 1 || kb[replyC.ID] != "reply" {
		t.Fatalf("B 的提醒不对：%v", kb)
	}
	kc := kinds(c.ID)
	if len(kc) != 2 || kc[mentionC.ID] != "mention" || kc[replyB.ID] != "reply" {
		t.Fatalf("C 的提醒不对：%v", kc)
	}

	// 修改时新 @ 了 A：多一条 @ 提醒；再改一次、仍然 @ A 不再重复提醒
	edit1 := add(b, &Record{Type: "edit", Ann: byB.ID, AuthorName: "B", Content: &Content{Text: "@A 改了", Mentions: []string{a.ID}}})
	add(b, &Record{Type: "edit", Ann: byB.ID, AuthorName: "B", Content: &Content{Text: "@A 又改了", Mentions: []string{a.ID}}})
	ka = kinds(a.ID)
	if ka[edit1.ID] != "mention" || ka[byB.ID] != "comment" || len(ka) != 5 {
		t.Fatalf("修改时新 @ 的提醒不对：%v", ka)
	}
	for _, it := range st.Inbox(a.ID) {
		if it.ID == byB.ID && it.Text != "@A 又改了" {
			t.Fatalf("提醒里应该显示最新的内容：%q", it.Text)
		}
	}

	// 删除以后，这条批注的提醒都没了
	add(b, &Record{Type: "delete", Ann: byB.ID, AuthorName: "B"})
	ka = kinds(a.ID)
	if _, ok := ka[byB.ID]; ok {
		t.Fatal("删除的批注不应该再提醒")
	}
	if _, ok := ka[edit1.ID]; ok {
		t.Fatal("删除的批注，修改时的 @ 也不应该再提醒")
	}
	// 原批注删了，回复还在：靠回复里带的被回复人判断
	if kinds(b.ID)[replyC.ID] != "reply" {
		t.Fatal("原批注删了以后，回复我的仍然要提醒")
	}

	if got := st.People()[c.ID].Name; got != "C" {
		t.Fatalf("记录里的昵称不对：%q", got)
	}
	if got := st.People()[a.ID].Name; got != "A" {
		t.Fatalf("视频主人的昵称不对：%q", got)
	}
}

func TestMentionsMustBeDeviceIDs(t *testing.T) {
	a := newTestIdentity(t)
	st, _ := Open(t.TempDir())
	ann := AuthorTag(a.ID) + RandomHex(8)
	r := signed(t, a, &Record{ID: ann, Type: "create", Ann: ann, Clock: 1,
		Video: &VideoRef{FP: "f", Owner: a.ID}, Content: &Content{Text: "x", Mentions: []string{"不是设备"}}})
	if _, err := st.Add(r); err == nil {
		t.Fatal("@ 的不是设备 ID 时应该拒绝")
	}
}
