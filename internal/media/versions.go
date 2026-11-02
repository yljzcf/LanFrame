package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"lanreview/internal/store"
)

// 版本组只记在视频分享者自己的电脑上（批注文件夹里的 versions.json，不参与同步），
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
	FileVer  int    `json:"fileVer,omitempty"` // 文件名里的版本号；和 Version 不一样说明文件名的号偏小，见 numberedVersions
	Missing  bool   `json:"missing,omitempty"` // 文件已经不在了，只剩批注
	Suggest  string `json:"suggest,omitempty"` // 只给本机：疑似哪个视频的新版本
	// 备份（见 backups.go）：这是 Origin 的视频，存在列出它的这台电脑上。0.3.x 不认这两个字段，当成这台电脑自己的视频
	Origin     string `json:"origin,omitempty"`
	OriginName string `json:"originName,omitempty"`
	// 下面三个只给本机网页（见 backups.go 的 videoView）
	Holders []string `json:"holders,omitempty"` // 现在在线、存着这个视频的备份的人，原主人不在时从他们那里播放
	Backup  bool     `json:"backup,omitempty"`  // 原主人那边已经没有这个文件（或者从没见过原主人），列表里显示的是备份版
	Local   bool     `json:"local,omitempty"`   // 本机存着这个视频的备份
}

type Versions struct {
	mu        sync.Mutex
	path      string
	Groups    []*VersionGroup   `json:"groups"`
	Suggest   map[string]string `json:"suggest"`   // 新文件指纹 -> 建议归入的版本组
	Dismissed map[string]bool   `json:"dismissed"` // 已经回答过"不是"的指纹
}

func LoadVersions(path string) *Versions {
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
	store.WriteFileAtomic(vs.path, data)
}

// 去掉文件名里常见的版本标记，用来判断两个文件是不是同一个片子的不同版本。
// v02 这类标记前面必须是开头或分隔符，避免把 MV01 里的 V01 当成版本号。
var reVersionMark = regexp.MustCompile(`(?i)(^|[\s_\-.])(v|ver|version)\s*\d+|版本\s*\d+|第\s*\d+\s*版|\(\d+\)|（\d+）|final|终版|定稿|最终版|副本|copy|[\s_\-.]+`)

func baseKey(name string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return strings.ToLower(reVersionMark.ReplaceAllString(name, ""))
}

// 文件名末尾的版本号，比如 _v03、 V3、-ver2；前面必须是开头或分隔符。
// 超过 3 位的数字（v20241031 这种日期）不算版本号。
var reFileVersion = regexp.MustCompile(`(?i)(^|[\s_\-.])(?:v|ver|version)\s*(\d{1,3})$`)

// FileVersion 是文件名末尾的版本号，没有时是 0。
func FileVersion(name string) int {
	m := reFileVersion.FindStringSubmatch(strings.TrimSuffix(name, filepath.Ext(name)))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[2])
	return n
}

// WithVersion 把文件名（不含扩展名）末尾的版本号换成 n，比如 X_v02 → X_v06；没有版本号的补上 _v06。
func WithVersion(base string, n int) string {
	if reFileVersion.MatchString(base) {
		return reFileVersion.ReplaceAllString(base, fmt.Sprintf("${1}v%02d", n))
	}
	return fmt.Sprintf("%s_v%02d", base, n)
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
		vs.Groups = append(vs.Groups, &VersionGroup{ID: store.RandomHex(8), Versions: []*VideoInfo{&c}})
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
		to = &VersionGroup{ID: store.RandomHex(8)}
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

// Numbered 是版本组里的一个版本和它的版本号。
type Numbered struct {
	*VideoInfo
	Version int
}

// Members 是版本组里的所有版本和它们的版本号，从旧到新。
func (vs *Versions) Members(group string) ([]Numbered, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	g := vs.groupByIDLocked(group)
	if g == nil {
		return nil, errors.New("找不到这个视频")
	}
	return numberedVersions(g), nil
}

// VersionOf 是自己的视频里这个文件的版本号；备份不在自己的版本组里，找不到。
func (vs *Versions) VersionOf(fp string) (int, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	g, _ := vs.groupOfLocked(fp)
	if g == nil {
		return 0, errors.New("找不到这个视频")
	}
	for _, n := range numberedVersions(g) {
		if n.FP == fp {
			return n.Version, nil
		}
	}
	return 0, errors.New("找不到这个视频")
}

// numberedVersions 按文件修改时间从旧到新编版本号。文件名里带版本号、并且不比"上一个版本的号 + 1"小的，
// 照文件名的号（可以跳号，只有一个 X_v03 就是 v03）；没带号或者号偏小的，用"上一个版本的号 + 1"，
// 带了号却偏小的，网页上提示改正文件名。"上一个版本的号"是它实际编到的号（可能来自文件名），
// 不是排第几个：按排第几个算，X_v05（旧）、X_v02（新）会编成 v05、v02，新的反而号小。
// 这样号从旧到新一定递增、不会重号，最新的版本号最大。
func numberedVersions(g *VersionGroup) []Numbered {
	vers := append([]*VideoInfo{}, g.Versions...)
	sort.SliceStable(vers, func(i, j int) bool { return vers[i].ModTime < vers[j].ModTime })
	out := make([]Numbered, len(vers))
	next := 1
	for i, v := range vers {
		n := FileVersion(v.Name)
		if n < next {
			n = next
		}
		out[i] = Numbered{v, n}
		next = n + 1
	}
	return out
}

// Listing 生成视频列表：每组的版本号见 numberedVersions。
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
		vers := numberedVersions(g)
		for _, e := range vers {
			v, missing := now[e.FP], false
			if v == nil {
				if !hasAnn[e.FP] {
					continue
				}
				v, missing = e.VideoInfo, true
			}
			entry := &VideoEntry{VideoInfo: *v, Group: g.ID, Title: g.Name, Version: e.Version, Versions: len(vers),
				FileVer: FileVersion(e.Name), Missing: missing}
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
	vers := numberedVersions(g)
	if len(vers) == 0 {
		return ""
	}
	return vers[len(vers)-1].Name
}
