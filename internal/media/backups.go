package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lanreview/internal/store"
)

// 备份：从同事那里拿来、放在"备份"里的视频。内容和原主人的一模一样（指纹相同），
// 所以原主人不在时同事也能从这里播放，批注也是同一套。列表里它归在原主人名下，
// 不算自己的视频，不进自己的版本组。记录存在批注文件夹的 backups.json，只在自己电脑上，不参与同步。

type Backup struct {
	Origin     string     `json:"origin"`     // 原主人的设备 ID
	OriginName string     `json:"originName"` // 原主人的昵称
	Entry      VideoEntry `json:"entry"`      // 原主人列表里的这一项（名称、版本组、版本号），原主人在线时会刷新
	Saved      int64      `json:"saved"`
}

type Backups struct {
	mu   sync.Mutex
	path string
	m    map[string]*Backup // 指纹 -> 备份
}

func LoadBackups(path string) *Backups {
	b := &Backups{path: path, m: map[string]*Backup{}}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &b.m)
	}
	if b.m == nil {
		b.m = map[string]*Backup{}
	}
	return b
}

func (b *Backups) saveLocked() {
	data, _ := json.MarshalIndent(b.m, "", "  ")
	store.WriteFileAtomic(b.path, data)
}

func (b *Backups) Has(fp string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[fp] != nil
}

// snapshot 是存进备份记录的列表项：去掉只对原主人有意义、或者只在显示时才算的字段。
func snapshot(e VideoEntry) VideoEntry {
	e.Origin, e.OriginName, e.Holders, e.Suggest, e.Missing, e.Backup, e.Local = "", "", nil, "", false, false, false
	return e
}

// Add 记下一个备份。
func (b *Backups) Add(fp, origin, originName string, e VideoEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[fp] = &Backup{Origin: origin, OriginName: originName, Entry: snapshot(e), Saved: time.Now().UnixMilli()}
	b.saveLocked()
}

// Refresh 用原主人最新的视频列表更新备份里记的名称和版本，比如原主人改了名、多了版本。
func (b *Backups) Refresh(origin, originName string, entries []*VideoEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for _, e := range entries {
		rec := b.m[e.FP]
		if rec == nil || rec.Origin != origin || e.Origin != "" || e.Missing {
			continue
		}
		fresh := snapshot(*e)
		old, _ := json.Marshal(rec.Entry)
		now, _ := json.Marshal(fresh)
		if string(old) != string(now) || rec.OriginName != originName {
			rec.Entry, rec.OriginName = fresh, originName
			changed = true
		}
	}
	if changed {
		b.saveLocked()
	}
}

// Split 把扫描到的视频分成自己的和备份。
func (b *Backups) Split(videos []*VideoInfo) (own, backups []*VideoInfo) {
	if b == nil {
		return videos, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, v := range videos {
		if b.m[v.FP] != nil {
			backups = append(backups, v)
		} else {
			own = append(own, v)
		}
	}
	return own, backups
}

// Entries 是本机有文件的备份的列表项：名称和版本照原主人的，视频信息用本机文件的（内容一样）。
func (b *Backups) Entries(present []*VideoInfo) []*VideoEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []*VideoEntry{}
	for _, v := range present {
		rec := b.m[v.FP]
		if rec == nil {
			continue
		}
		e := rec.Entry
		e.VideoInfo = *v
		e.Name, e.Dir = rec.Entry.Name, rec.Entry.Dir
		e.Origin, e.OriginName = rec.Origin, rec.OriginName
		out = append(out, &e)
	}
	return out
}

// IsVideoName 判断文件名是不是程序能播放的视频。
func IsVideoName(name string) bool {
	return videoExts[strings.ToLower(filepath.Ext(name))]
}
