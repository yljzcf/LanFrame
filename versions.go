package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// 版本组只记在视频分享者自己的电脑上（批注同步文件夹里的 versions.json，不参与同步），
// 随视频列表一起发给同事。一个版本 = 一个指纹，每个版本的批注各自独立。

type VersionGroup struct {
	ID       string       `json:"id"`
	Name     string       `json:"name,omitempty"` // 分享者起的视频名称，没起时显示最新版本的文件名
	Versions []*VideoInfo `json:"versions"`
}

// VideoEntry 是发给网页和同事的视频列表项：视频信息加上它在版本组里的位置。
type VideoEntry struct {
	VideoInfo
	Group    string `json:"group"`
	Title    string `json:"title,omitempty"` // 视频名称
	Version  int    `json:"version"`
	Versions int    `json:"versions"`
	Missing  bool   `json:"missing,omitempty"` // 文件已经不在了，只剩批注
	Suggest  string `json:"suggest,omitempty"` // 只给本机：疑似哪个视频的新版本
}

type Versions struct {
	mu        sync.Mutex
	path      string
	Groups    []*VersionGroup   `json:"groups"`
	Suggest   map[string]string `json:"suggest"`   // 新文件指纹 -> 建议归入的版本组
	Dismissed map[string]bool   `json:"dismissed"` // 已经回答过"不是"的指纹
}

func loadVersions(path string) *Versions {
	vs := &Versions{path: path}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, vs)
	}
	if vs.Suggest == nil {
		vs.Suggest = map[string]string{}
	}
	if vs.Dismissed == nil {
		vs.Dismissed = map[string]bool{}
	}
	return vs
}

func (vs *Versions) saveLocked() {
	data, _ := json.MarshalIndent(vs, "", "  ")
	writeFileAtomic(vs.path, data)
}

// 去掉文件名里常见的版本标记，用来判断两个文件是不是同一个片子的不同版本。
// v02 这类标记前面必须是开头或分隔符，避免把 MV01 里的 V01 当成版本号。
var reVersionMark = regexp.MustCompile(`(?i)(^|[\s_\-.])(v|ver|version)\s*\d+|版本\s*\d+|第\s*\d+\s*版|\(\d+\)|（\d+）|final|终版|定稿|最终版|副本|copy|[\s_\-.]+`)

func baseKey(name string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return strings.ToLower(reVersionMark.ReplaceAllString(name, ""))
}

func (vs *Versions) groupOfLocked(fp string) (*VersionGroup, int) {
	for _, g := range vs.Groups {
		for i, v := range g.Versions {
			if v.FP == fp {
				return g, i
			}
		}
	}
	return nil, -1
}

func (vs *Versions) groupByIDLocked(id string) *VersionGroup {
	for _, g := range vs.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// Update 把扫描到的新文件各自放进一个新组；如果名字和已有的视频相近，记下"疑似新版本"等分享者确认。
// hasAnn 是有批注的指纹：文件已经不在、又没有批注的旧版本没有保留的意义。
func (vs *Versions) Update(videos []*VideoInfo, hasAnn map[string]bool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	changed := false
	present := map[string]bool{}
	for _, v := range videos {
		present[v.FP] = true
	}
	gone := func(e *VideoInfo) bool { return !present[e.FP] && !hasAnn[e.FP] }
	sorted := append([]*VideoInfo{}, videos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ModTime < sorted[j].ModTime })
	for _, v := range sorted {
		c := *v
		if g, i := vs.groupOfLocked(v.FP); g != nil {
			if old := g.Versions[i]; old.Name != v.Name || old.Dir != v.Dir {
				g.Versions[i] = &c
				changed = true
			}
			continue
		}
		// 同一位置的旧文件已经不在、也没有批注：当作同一个视频被覆盖了，直接顶替，不用问
		if g, i := vs.samePathLocked(v, gone); g != nil {
			old := g.Versions[i].FP
			g.Versions[i] = &c
			if s, ok := vs.Suggest[old]; ok {
				vs.Suggest[v.FP] = s
			}
			delete(vs.Suggest, old)
			changed = true
			continue
		}
		var cand *VersionGroup
		if key := baseKey(v.Name); key != "" {
			for _, g := range vs.Groups {
				// 所有版本都已不在、也没有批注的组在列表里看不到，不拿来比
				visible := false
				for _, e := range g.Versions {
					visible = visible || !gone(e)
				}
				if !visible {
					continue
				}
				if g.Name != "" && baseKey(g.Name) == key {
					cand = g
				}
				for _, e := range g.Versions {
					if baseKey(e.Name) == key {
						cand = g
					}
				}
			}
		}
		vs.Groups = append(vs.Groups, &VersionGroup{ID: randomHex(8), Versions: []*VideoInfo{&c}})
		if cand != nil && !vs.Dismissed[v.FP] {
			vs.Suggest[v.FP] = cand.ID
		}
		changed = true
	}
	if changed {
		vs.saveLocked()
	}
}

func (vs *Versions) samePathLocked(v *VideoInfo, gone func(*VideoInfo) bool) (*VersionGroup, int) {
	for _, g := range vs.Groups {
		for i, e := range g.Versions {
			if e.Dir == v.Dir && e.Name == v.Name && gone(e) {
				return g, i
			}
		}
	}
	return nil, -1
}

// Move 把一个版本挪进另一个版本组；group 为空表示单独成为一个视频。
func (vs *Versions) Move(fp, group string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	from, i := vs.groupOfLocked(fp)
	if from == nil {
		return errors.New("找不到这个视频")
	}
	var to *VersionGroup
	if group != "" {
		if to = vs.groupByIDLocked(group); to == nil {
			return errors.New("目标视频不存在")
		}
		if to == from {
			return nil
		}
	}
	v := from.Versions[i]
	from.Versions = append(from.Versions[:i], from.Versions[i+1:]...)
	if to == nil {
		to = &VersionGroup{ID: randomHex(8)}
		vs.Groups = append(vs.Groups, to)
	}
	to.Versions = append(to.Versions, v)
	if len(from.Versions) == 0 {
		for k, g := range vs.Groups {
			if g == from {
				vs.Groups = append(vs.Groups[:k], vs.Groups[k+1:]...)
				break
			}
		}
	}
	delete(vs.Suggest, fp)
	vs.Dismissed[fp] = true
	vs.saveLocked()
	return nil
}

// Answer 回答"是不是新版本"的提示。
func (vs *Versions) Answer(fp string, yes bool) error {
	vs.mu.Lock()
	group, ok := vs.Suggest[fp]
	if !ok {
		vs.mu.Unlock()
		return errors.New("这个提示已经处理过了")
	}
	if !yes {
		delete(vs.Suggest, fp)
		vs.Dismissed[fp] = true
		vs.saveLocked()
		vs.mu.Unlock()
		return nil
	}
	vs.mu.Unlock()
	return vs.Move(fp, group)
}

// SetName 给视频（整个版本组）起名称。
func (vs *Versions) SetName(group, name string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	g := vs.groupByIDLocked(group)
	if g == nil {
		return errors.New("找不到这个视频")
	}
	g.Name = name
	vs.saveLocked()
	return nil
}

// Members 是版本组里的所有版本，按版本号排好（第 i 个就是 v(i+1)）。
func (vs *Versions) Members(group string) ([]*VideoInfo, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	g := vs.groupByIDLocked(group)
	if g == nil {
		return nil, errors.New("找不到这个视频")
	}
	return sortedVersions(g), nil
}

// sortedVersions 按文件修改时间排版本，版本号就是排序后的位置。
func sortedVersions(g *VersionGroup) []*VideoInfo {
	vers := append([]*VideoInfo{}, g.Versions...)
	sort.SliceStable(vers, func(i, j int) bool { return vers[i].ModTime < vers[j].ModTime })
	return vers
}

// Listing 生成视频列表：每组按文件修改时间编 v1、v2……
// 文件已经不在、但还有批注的旧版本也列出来，方便对照之前的意见。
func (vs *Versions) Listing(present []*VideoInfo, hasAnn map[string]bool) []*VideoEntry {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	now := map[string]*VideoInfo{}
	for _, v := range present {
		now[v.FP] = v
	}
	out := []*VideoEntry{}
	for _, g := range vs.Groups {
		vers := sortedVersions(g)
		for i, e := range vers {
			v, missing := now[e.FP], false
			if v == nil {
				if !hasAnn[e.FP] {
					continue
				}
				v, missing = e, true
			}
			entry := &VideoEntry{VideoInfo: *v, Group: g.ID, Title: g.Name, Version: i + 1, Versions: len(vers), Missing: missing}
			if target := vs.groupByIDLocked(vs.Suggest[e.FP]); target != nil && target != g {
				entry.Suggest = displayName(target)
			}
			out = append(out, entry)
		}
	}
	return out
}

// displayName 是视频名称；没起名时用最新版本的文件名。
func displayName(g *VersionGroup) string {
	if g.Name != "" {
		return g.Name
	}
	vers := sortedVersions(g)
	if len(vers) == 0 {
		return ""
	}
	return vers[len(vers)-1].Name
}
