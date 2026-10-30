package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func signed(t *testing.T, id *Identity, r *Record) *Record {
	t.Helper()
	r.Author = id.ID
	if err := id.sign(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOnlyAuthorCanEditOrDelete(t *testing.T) {
	author, other := newTestIdentity(t), newTestIdentity(t)
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ann := authorTag(author.ID) + randomHex(8)
	create := signed(t, author, &Record{ID: ann, Type: "create", Ann: ann, Clock: 1,
		Video: &VideoRef{FP: "f", Owner: author.ID}, Content: &Content{Text: "原文"}})
	if ok, err := st.Add(create); !ok || err != nil {
		t.Fatalf("作者创建失败：%v", err)
	}
	forgedEdit := signed(t, other, &Record{ID: randomHex(16), Type: "edit", Ann: ann, Clock: 2, Content: &Content{Text: "偷改"}})
	if _, err := st.Add(forgedEdit); err == nil {
		t.Fatal("别人的修改不应该被接受")
	}
	forgedDelete := signed(t, other, &Record{ID: randomHex(16), Type: "delete", Ann: ann, Clock: 2})
	if _, err := st.Add(forgedDelete); err == nil {
		t.Fatal("别人的删除不应该被接受")
	}
	// 签名后再改内容，签名就对不上了
	tampered := signed(t, author, &Record{ID: randomHex(16), Type: "edit", Ann: ann, Clock: 3, Content: &Content{Text: "正常修改"}})
	tampered.Content.Text = "被篡改"
	if _, err := st.Add(tampered); err == nil {
		t.Fatal("被篡改的记录不应该被接受")
	}
	items, _ := st.Annotations("f", author.ID)
	if len(items) != 1 || items[0].Content.Text != "原文" {
		t.Fatalf("批注状态不对：%+v", items)
	}
}

func TestDeletedAnnotationStaysDeleted(t *testing.T) {
	author := newTestIdentity(t)
	share := t.TempDir()
	st, _ := openStore(share)
	ann := authorTag(author.ID) + randomHex(8)
	create := signed(t, author, &Record{ID: ann, Type: "create", Ann: ann, Clock: 1,
		Video: &VideoRef{FP: "f", Owner: author.ID}, Content: &Content{Text: "x"}})
	st.Add(create)
	st.Add(signed(t, author, &Record{ID: randomHex(16), Type: "delete", Ann: ann, Clock: 2, Video: create.Video}))
	if _, err := os.Stat(st.RecordPath(ann)); !os.IsNotExist(err) {
		t.Fatal("删除后 create 记录文件应该被清掉")
	}
	// 离线很久的电脑带着旧记录回来
	if ok, _ := st.Add(create); ok {
		t.Fatal("被删除的批注不应该复活")
	}
	// 有人把旧文件手动放回文件夹，重新打开时也会被清掉
	writeJSONFile(t, filepath.Join(share, syncDirName, "records", ann+".json"), create)
	st2, _ := openStore(share)
	items, deleted := st2.Annotations("f", author.ID)
	if len(items) != 0 || len(deleted) != 1 {
		t.Fatalf("重新打开后状态不对：items=%d deleted=%d", len(items), len(deleted))
	}
	if _, err := os.Stat(st.RecordPath(ann)); !os.IsNotExist(err) {
		t.Fatal("手动放回的旧记录应该被清掉")
	}
}

func writeJSONFile(t *testing.T, path string, r *Record) {
	t.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
