package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"lanreview/internal/media"
	"lanreview/internal/platform"
	"lanreview/internal/store"
)

// 备份：从同事那里拿来、放在"备份"里的视频（记录见 internal/media/backups.go）。
// 内容和原主人的一模一样，原主人不在时同事也能从备份播放，批注按指纹对应，也是同一套。
// 发给同事的视频列表（/peer/videos）里，备份带着原主人（origin）；本机网页上把它归到原主人名下，不另算一个视频。

// noteBackup 在一个文件下完、改回原名之前调用：拿来的是同事列表里的视频时，记成原主人视频的备份。
// 先记再改名，免得扫描先把它当成自己的新视频。转手的备份沿用原来的原主人。
func (a *App) noteBackup(peerID, local, part string, size int64) {
	bk := a.getBackups()
	if bk == nil || !media.IsVideoName(local) {
		return
	}
	fp, err := media.Fingerprint(part, size)
	if err != nil || bk.Has(fp) || a.videos.Get(fp) != nil {
		return // 已经记过；或者自己本来就有这个视频，还算自己的
	}
	for _, p := range a.peers.Snapshot() {
		if p.ID != peerID {
			continue
		}
		for _, v := range p.Videos {
			if v.FP != fp || v.Missing {
				continue
			}
			origin, name := p.ID, p.Name
			if v.Origin != "" {
				origin, name = v.Origin, v.OriginName
			}
			if origin == a.me().ID {
				return // 自己的视频，从别人的备份又拿了回来
			}
			bk.Add(fp, origin, name, *v)
			log.Printf("%s 是 %s 的视频的备份", local, name)
			return
		}
	}
}

// listing 是本机分享的视频列表：自己的带上版本信息，备份带上原主人。
func (a *App) listing() []*media.VideoEntry {
	vs, st, bk := a.getVersions(), a.getStore(), a.getBackups()
	if vs == nil || st == nil {
		return []*media.VideoEntry{}
	}
	own, backups := bk.Split(a.videos.List())
	out := []*media.VideoEntry{}
	for _, e := range vs.Listing(own, st.AnnotatedFPs()) {
		if !bk.Has(e.FP) { // versions.json 里可能还留着后来才成为备份的视频
			out = append(out, e)
		}
	}
	if bk != nil {
		out = append(out, bk.Entries(backups)...)
	}
	return out
}

// videoView 是给本机网页看的视频列表：备份（自己的和同事的）归到原主人名下，在线的备份者记在 holders 里。
// 优先显示原主人的文件；原主人那边已经删了（文件已不在），或者原主人列表里没有、这次运行里从没见过原主人时，
// 显示备份版（backup），不替原主人复原文件。/peer/videos 和播放仍然用原始列表，不受影响。
func (a *App) videoView() (mine []*media.VideoEntry, peers []Peer) {
	me := a.me().ID
	type held struct {
		e      *media.VideoEntry
		holder string
		online bool
		local  bool // 本机存着的
	}
	var backups []held
	mine = []*media.VideoEntry{}
	for _, e := range a.listing() {
		if e.Origin != "" {
			backups = append(backups, held{e, "本机", true, true})
		} else {
			mine = append(mine, e)
		}
	}
	peers = []Peer{}
	for _, p := range a.peers.Snapshot() {
		// 导入身份后，旧电脑如果还开着会用同一个身份出现，不当成同事显示
		if p.ID == me {
			continue
		}
		own := []*media.VideoEntry{}
		for _, e := range p.Videos {
			if e.Origin != "" {
				backups = append(backups, held{e, p.Name, p.Online, false})
				continue
			}
			c := *e // Snapshot 里的列表项是共享的，改之前先复制
			c.Holders, c.Backup, c.Local = nil, false, false
			own = append(own, &c)
		}
		if p.Videos != nil {
			p.Videos = own
		}
		peers = append(peers, p)
	}
	listOf := func(origin, name string) *[]*media.VideoEntry {
		if origin == me {
			return &mine
		}
		for i := range peers {
			if peers[i].ID == origin {
				return &peers[i].Videos
			}
		}
		peers = append(peers, Peer{ID: origin, Name: name, Code: store.DeviceCode(origin), Videos: []*media.VideoEntry{}})
		return &peers[len(peers)-1].Videos
	}
	for _, b := range backups {
		list := listOf(b.e.Origin, b.e.OriginName)
		var found *media.VideoEntry
		for _, e := range *list {
			if e.FP == b.e.FP {
				found = e
				break
			}
		}
		switch {
		case found == nil: // 原主人列表里没有：显示备份版
			c := *b.e
			c.Origin, c.OriginName, c.Holders, c.Backup = "", "", nil, true
			*list = append(*list, &c)
			found = &c
		case found.Missing: // 原主人删了：仍然在原主人名下，换成备份版
			found.Missing, found.Backup = false, true
		}
		if b.online {
			found.Holders = append(found.Holders, b.holder)
		}
		found.Local = found.Local || b.local
	}
	return mine, peers
}

// startVideoBackup 把同事的一个视频备份到本机的"备份/原主人/…"里（列表上的"备份视频"）。
// 按指纹拿：原主人在线、文件还在就从他那里拿，不然从在线的、存着备份的同事那里拿。
func (a *App) startVideoBackup(owner, fp string) error {
	if !media.ValidFP(fp) || owner == "" {
		return errors.New("视频信息不完整")
	}
	if owner == a.me().ID {
		return errors.New("这是你自己的视频")
	}
	if a.videos.Get(fp) != nil {
		return errors.New("本机已经有这个视频了")
	}
	root, err := a.backupRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return localError(root, err)
	}
	// 这个视频的名称和位置：原主人列表里的优先，没有的话用备份者列表里记的（照原主人的）
	var entry *media.VideoEntry
	originName := ""
	peers := a.peers.Snapshot()
	for _, p := range peers {
		if p.ID != owner {
			continue
		}
		originName = p.Name
		for _, v := range p.Videos {
			if v.FP == fp && !v.Missing && v.Origin == "" {
				entry = v
			}
		}
	}
	for _, p := range peers {
		for _, v := range p.Videos {
			if entry == nil && v.FP == fp && !v.Missing && v.Origin == owner {
				entry = v
				if originName == "" {
					originName = v.OriginName
				}
			}
		}
	}
	if entry == nil {
		return errors.New("原主人和存着备份的同事都不在线，暂时拿不到这个视频")
	}
	rel := path.Join(entry.Dir, entry.Name)
	local := backupPath(root, originName, rel)
	if free, err := platform.FreeSpace(root); err == nil && uint64(entry.Size) > free {
		return fmt.Errorf("%s 所在的磁盘剩余空间不够：要下载 %s，只剩 %s", root, humanSize(entry.Size), humanSize(int64(free)))
	}

	a.dl.mu.Lock()
	defer a.dl.mu.Unlock()
	for _, j := range a.dl.jobs {
		if j.FP == fp && !j.finished() {
			return errors.New("这个视频已经在备份了，右上角\"下载\"里能看进度")
		}
	}
	taken := a.dl.takenPathsLocked()
	f := &dlFile{Path: rel, Local: local, Size: entry.Size, MTime: entry.ModTime}
	switch {
	case sameFile(local, f.Size, f.MTime) && !taken[strings.ToLower(local)]:
		f.Done = true // 文件已经在了（之前拿过，还没扫描到），直接记成备份
		f.got.Store(f.Size)
		if bk := a.getBackups(); bk != nil {
			bk.Add(fp, owner, originName, *entry)
		}
	case taken[strings.ToLower(local)] || fileExists(local):
		f.Local = uniqueName(filepath.Dir(local), filepath.Base(local), false, taken)
	}
	j := &dlJob{
		ID: store.RandomHex(8), Peer: owner, PeerName: originName, Name: filepath.Base(f.Local), Local: f.Local,
		FP: fp, Created: time.Now().UnixMilli(), Files: []*dlFile{f},
	}
	if f.Done {
		j.doneAt = time.Now()
	}
	a.dl.jobs = append(a.dl.jobs, j)
	a.dl.saveLocked()
	a.dl.kick()
	log.Printf("开始备份 %s 的视频 %s（%s）到 %s", originName, entry.Name, humanSize(entry.Size), f.Local)
	return nil
}

func (a *App) apiBackupVideo(w http.ResponseWriter, r *http.Request) {
	var body struct{ Owner, FP string }
	if err := readJSON(r, &body); err != nil {
		httpErr(w, errors.New("请求不对"), http.StatusBadRequest)
		return
	}
	if err := a.startVideoBackup(body.Owner, body.FP); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
