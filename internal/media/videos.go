package media

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lanreview/internal/store"
)

type VideoInfo struct {
	FP       string  `json:"fp"`
	Name     string  `json:"name"`
	Dir      string  `json:"dir"` // 共享文件夹里的相对目录
	Size     int64   `json:"size"`
	ModTime  int64   `json:"modTime"`
	FPS      float64 `json:"fps"`
	Frames   int     `json:"frames"`
	Duration float64 `json:"duration"`
	VFR      bool    `json:"vfr"`
	Codec    string  `json:"codec"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	path     string
}

type VideoLib struct {
	mu   sync.Mutex
	list []*VideoInfo
	byFP map[string]*VideoInfo

	// 下面两个只在扫描时用，由 scanMu 保护
	scanMu  sync.Mutex
	cache   map[string]*VideoInfo // 路径+大小+修改时间 -> 已算好的信息，避免重复算指纹；只留最近一次扫描到的
	pending map[string]string     // 还没确认写完的文件：路径 -> 上次看到的大小+修改时间
}

func NewVideoLib() *VideoLib {
	return &VideoLib{byFP: map[string]*VideoInfo{}, cache: map[string]*VideoInfo{}, pending: map[string]string{}}
}

// 文件还在导出或拷贝时不能算指纹，否则每个写到一半的状态都会被当成一个新版本。
// 修改时间超过一分钟的直接认为写完了；更新的文件要连续两次扫描大小和修改时间都不变，并且 10 秒内没再改动。
func (l *VideoLib) settled(path string, info fs.FileInfo, next map[string]string) bool {
	if info.Size() == 0 {
		return false
	}
	age := time.Since(info.ModTime())
	if age > time.Minute {
		return true
	}
	sig := fmt.Sprintf("%d|%d", info.Size(), info.ModTime().UnixNano())
	next[path] = sig
	return l.pending[path] == sig && age > 10*time.Second
}

var (
	videoExts = map[string]bool{".mp4": true, ".m4v": true, ".mov": true}
	reFP      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func (l *VideoLib) Scan(root string) {
	l.scanMu.Lock()
	defer l.scanMu.Unlock()
	var list []*VideoInfo
	seen := map[string]bool{}
	nextCache := map[string]*VideoInfo{}
	nextPending := map[string]string{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == store.SyncDirName || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !videoExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		key := fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano())
		v := l.cache[key]
		if v == nil {
			if !l.settled(path, info, nextPending) {
				return nil
			}
			fp, err := fingerprint(path, info.Size())
			if err != nil {
				log.Printf("算指纹失败 %s：%v", path, err)
				return nil
			}
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			v = &VideoInfo{FP: fp, Name: name, Dir: filepath.ToSlash(rel), Size: info.Size(),
				ModTime: info.ModTime().UnixMilli(), path: path}
			if m, err := probeMP4(path); err == nil {
				v.FPS, v.Frames, v.Duration, v.VFR = m.fps, m.frames, m.duration, m.vfr
				v.Codec, v.Width, v.Height = m.codec, m.width, m.height
			} else {
				log.Printf("读取视频信息失败 %s：%v", path, err)
			}
		}
		nextCache[key] = v
		// 内容完全相同的两个文件只列一个
		if !seen[v.FP] {
			seen[v.FP] = true
			list = append(list, v)
		}
		return nil
	})
	l.cache, l.pending = nextCache, nextPending
	sort.Slice(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].ModTime < list[j].ModTime
	})
	byFP := map[string]*VideoInfo{}
	for _, v := range list {
		byFP[v.FP] = v
	}
	l.mu.Lock()
	l.list, l.byFP = list, byFP
	l.mu.Unlock()
}

func (l *VideoLib) List() []*VideoInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*VideoInfo{}, l.list...)
}

func (l *VideoLib) Get(fp string) *VideoInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byFP[fp]
}

// fingerprint 用"文件大小 + 开头、中间、结尾各 4MB"算 SHA-256，大文件也几乎瞬间算完。
func fingerprint(path string, size int64) (string, error) {
	const chunk = 4 << 20
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	binary.Write(h, binary.BigEndian, size)
	if size <= 3*chunk {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else {
		for _, off := range []int64{0, size/2 - chunk/2, size - chunk} {
			if _, err := io.Copy(h, io.NewSectionReader(f, off, chunk)); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}
