package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const SyncDirName = "批注同步(别删)"

var (
	reRecordID  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reImageName = regexp.MustCompile(`^[0-9a-f]{64}\.(png|jpg|gif|webp)$`)
)

// Stroke 是画在画面上的一笔，坐标是相对画面宽高的 0~1。
type Stroke struct {
	Kind   string       `json:"kind"` // pen | arrow | rect | text
	Color  string       `json:"color"`
	Width  float64      `json:"width"` // 相对 1000 像素宽画面的线宽
	Points [][2]float64 `json:"points"`
	Text   string       `json:"text,omitempty"`
}

type Content struct {
	Frame       int      `json:"frame"`
	Seconds     float64  `json:"seconds"`
	Text        string   `json:"text"`
	Strokes     []Stroke `json:"strokes,omitempty"`
	Shot        string   `json:"shot,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

type VideoRef struct {
	FP        string  `json:"fp"`
	Name      string  `json:"name"`
	Owner     string  `json:"owner"`
	OwnerName string  `json:"ownerName"`
	FPS       float64 `json:"fps"`
}

// Record 是同步的最小单位，一条记录一个文件，只增不改。
// 删除批注也是新增一条 delete 记录。
type Record struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"` // create | edit | delete
	Ann        string    `json:"ann"`  // 批注 ID，等于 create 记录的 ID
	Clock      int64     `json:"clock"`
	Time       int64     `json:"time"`
	Author     string    `json:"author"`
	AuthorName string    `json:"authorName"`
	Video      *VideoRef `json:"video,omitempty"`
	Content    *Content  `json:"content,omitempty"`
	ReplyTo    string    `json:"replyTo,omitempty"` // 回复：被回复的批注 ID（只在 create 记录里）
	Quote      *Quote    `json:"quote,omitempty"`   // 回复时带上被回复的内容，原批注删了也有上下文
	Sig        string    `json:"sig,omitempty"`
}

type Quote struct {
	Author     string `json:"author"`
	AuthorName string `json:"authorName"`
	Text       string `json:"text"`
}

type RecordStub struct {
	ID   string `json:"id"`
	Ann  string `json:"ann"`
	Type string `json:"type"`
}

type HistItem struct {
	Time    int64   `json:"time"`
	Content Content `json:"content"`
}

type AnnView struct {
	ID         string     `json:"id"`
	Author     string     `json:"author"`
	AuthorName string     `json:"authorName"`
	Owner      string     `json:"owner"`
	Created    int64      `json:"created"`
	Content    Content    `json:"content"`
	History    []HistItem `json:"history"`
	CanEdit    bool       `json:"canEdit"`
	ReplyTo    string     `json:"replyTo,omitempty"`
	Quote      *Quote     `json:"quote,omitempty"`
}

type DeletedView struct {
	Ann        string `json:"ann"`
	AuthorName string `json:"authorName"`
	Time       int64  `json:"time"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	recs     map[string]*Record
	deleted  map[string]*Record // 批注 ID -> 有效的删除记录
	maxClock int64
	version  int64
}

func Open(shareDir string) (*Store, error) {
	// 共享文件夹本身不存在时不能替用户建（比如移动硬盘没接上，会在本机建出一个同名空文件夹）
	if st, err := os.Stat(shareDir); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fs.ErrNotExist
	}
	dir := filepath.Join(shareDir, SyncDirName)
	for _, sub := range []string{"records", "images"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, recs: map[string]*Record{}, deleted: map[string]*Record{}}
	entries, err := os.ReadDir(filepath.Join(dir, "records"))
	if err != nil {
		return nil, err
	}
	var loaded []*Record
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "records", e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(data, &r) != nil || validateRecord(&r) != nil || e.Name() != r.ID+".json" {
			log.Printf("忽略无效的批注记录：%s", e.Name())
			continue
		}
		loaded = append(loaded, &r)
	}
	// 先登记删除记录，再放其他记录，被删的批注不会再被加载回来
	sort.SliceStable(loaded, func(i, j int) bool { return loaded[i].Type == "delete" && loaded[j].Type != "delete" })
	for _, r := range loaded {
		if ok, _ := s.addLocked(r, false); !ok && s.deleted[r.Ann] != nil {
			os.Remove(s.RecordPath(r.ID))
		}
	}
	if len(s.deleted) > 0 {
		s.gcImagesLocked()
	}
	return s, nil
}

func validateRecord(r *Record) error {
	if !reRecordID.MatchString(r.ID) || !reRecordID.MatchString(r.Ann) {
		return errors.New("ID 格式不对")
	}
	if r.ReplyTo != "" && (r.Type != "create" || !reRecordID.MatchString(r.ReplyTo)) {
		return errors.New("回复记录不对")
	}
	switch r.Type {
	case "create":
		if r.ID != r.Ann || r.Video == nil || r.Content == nil || AuthorTag(r.Author) != r.Ann[:16] {
			return errors.New("create 记录不完整")
		}
	case "edit", "delete":
		// 只有作者本人能修改和删除，批注 ID 前半段就是作者标记
		if AuthorTag(r.Author) != r.Ann[:16] {
			return errors.New("只有作者本人能修改或删除批注")
		}
		if r.Type == "edit" && r.Content == nil {
			return errors.New("edit 记录没有内容")
		}
	default:
		return errors.New("未知的记录类型")
	}
	if r.Content != nil {
		for _, name := range append([]string{r.Content.Shot}, r.Content.Attachments...) {
			if name != "" && !reImageName.MatchString(name) {
				return errors.New("图片名不对")
			}
		}
	}
	if !verifyRecord(r) {
		return errors.New("签名无效")
	}
	return nil
}

// Add 校验并保存一条记录。返回 false 表示已存在或被拒绝（比如批注已被删除）。
func (s *Store) Add(r *Record) (bool, error) {
	if err := validateRecord(r); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addLocked(r, true)
}

func (s *Store) addLocked(r *Record, persist bool) (bool, error) {
	if _, ok := s.recs[r.ID]; ok {
		return false, nil
	}
	isDelete := r.Type == "delete"
	if s.deleted[r.Ann] != nil && !isDelete {
		return false, nil
	}
	if persist {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return false, err
		}
		if err := WriteFileAtomic(filepath.Join(s.dir, "records", r.ID+".json"), data); err != nil {
			return false, err
		}
	}
	s.recs[r.ID] = r
	if r.Clock > s.maxClock {
		s.maxClock = r.Clock
	}
	s.version++
	if isDelete && s.deleted[r.Ann] == nil {
		s.deleted[r.Ann] = r
		if persist {
			s.purgeLocked(r.Ann)
		}
	}
	return true, nil
}

// purgeLocked 清掉被删批注的所有版本，只留下删除记录，再清理没人用的图片。
func (s *Store) purgeLocked(ann string) {
	for id, r := range s.recs {
		if r.Ann == ann && r.Type != "delete" {
			os.Remove(filepath.Join(s.dir, "records", id+".json"))
			delete(s.recs, id)
		}
	}
	s.gcImagesLocked()
}

// GC 定期清理被删批注留下的图片。
func (s *Store) GC() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deleted) > 0 {
		s.gcImagesLocked()
	}
}

func (s *Store) gcImagesLocked() {
	used := s.referencedImagesLocked()
	entries, _ := os.ReadDir(filepath.Join(s.dir, "images"))
	for _, e := range entries {
		info, err := e.Info()
		// 刚上传、还没发布的图片先留着
		if err != nil || used[e.Name()] || time.Since(info.ModTime()) < 10*time.Minute {
			continue
		}
		os.Remove(filepath.Join(s.dir, "images", e.Name()))
	}
}

func (s *Store) referencedImagesLocked() map[string]bool {
	used := map[string]bool{}
	for _, r := range s.recs {
		if r.Content == nil {
			continue
		}
		if r.Content.Shot != "" {
			used[r.Content.Shot] = true
		}
		for _, a := range r.Content.Attachments {
			used[a] = true
		}
	}
	return used
}

func (s *Store) MissingImages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var missing []string
	for name := range s.referencedImagesLocked() {
		if _, err := os.Stat(s.ImagePath(name)); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

func (s *Store) NextClock() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxClock++
	return s.maxClock
}

func (s *Store) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *Store) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recs[id]
	return ok
}

func (s *Store) IsDeleted(ann string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[ann] != nil
}

func (s *Store) Get(id string) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs[id]
}

func (s *Store) List() []RecordStub {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordStub, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, RecordStub{r.ID, r.Ann, r.Type})
	}
	return out
}

func (s *Store) RecordPath(id string) string {
	return filepath.Join(s.dir, "records", id+".json")
}

func (s *Store) ImagePath(name string) string {
	return filepath.Join(s.dir, "images", name)
}

// PutImage 按内容哈希保存图片，相同的图片只存一份。
func (s *Store) PutImage(data []byte, ext string) (string, error) {
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + "." + ext
	path := s.ImagePath(name)
	if _, err := os.Stat(path); err == nil {
		return name, nil
	}
	return name, WriteFileAtomic(path, data)
}

func sortRecords(rs []*Record) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Clock != rs[j].Clock {
			return rs[i].Clock < rs[j].Clock
		}
		return rs[i].ID < rs[j].ID
	})
}

// Annotations 算出某个视频当前的批注：每条批注取作者最新的一次修改。
func (s *Store) Annotations(fp, me string) ([]AnnView, []DeletedView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []AnnView{}
	deleted := []DeletedView{}
	for ann, d := range s.deleted {
		if d.Video != nil && d.Video.FP == fp {
			deleted = append(deleted, DeletedView{ann, d.AuthorName, d.Time})
		}
	}
	byAnn := map[string][]*Record{}
	for _, r := range s.recs {
		if r.Type != "delete" {
			byAnn[r.Ann] = append(byAnn[r.Ann], r)
		}
	}
	for ann, rs := range byAnn {
		create := s.recs[ann]
		if create == nil || create.Type != "create" || create.Video.FP != fp {
			continue
		}
		sortRecords(rs)
		v := AnnView{
			ID: ann, Author: create.Author, AuthorName: create.AuthorName,
			Owner: create.Video.Owner, Created: create.Time, Content: *create.Content,
			History: []HistItem{{create.Time, *create.Content}},
			ReplyTo: create.ReplyTo, Quote: create.Quote,
		}
		for _, r := range rs {
			if r.Type != "edit" || r.Author != create.Author {
				continue
			}
			c := *r.Content
			c.Frame, c.Seconds, c.Shot = create.Content.Frame, create.Content.Seconds, create.Content.Shot
			v.Content = c
			v.History = append(v.History, HistItem{r.Time, c})
		}
		v.CanEdit = me == create.Author
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Content.Frame != items[j].Content.Frame {
			return items[i].Content.Frame < items[j].Content.Frame
		}
		return items[i].Created < items[j].Created
	})
	sort.Slice(deleted, func(i, j int) bool { return deleted[i].Time < deleted[j].Time })
	return items, deleted
}

// AnnotatedFPs 是有批注（不算已删除的）的视频指纹。
func (s *Store) AnnotatedFPs() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, r := range s.recs {
		if r.Type == "create" {
			out[r.Video.FP] = true
		}
	}
	return out
}

// AnnCounts 是每个视频的批注数（不算已删除的，也不算回复）。
func (s *Store) AnnCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, r := range s.recs {
		if r.Type == "create" && r.ReplyTo == "" {
			counts[r.Video.FP]++
		}
	}
	return counts
}

func WriteFileAtomic(path string, data []byte) error {
	tmp := path + "." + RandomHex(4) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
