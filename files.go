package main

import (
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"lanreview/internal/store"
)

// 共享文件夹里的所有文件都给同事浏览和下载（只读），同事的程序在后台下载，见 downloads.go。

// maxDeepFiles 是一次能下载的文件夹里最多有多少个文件，防止有人选了一个巨大的文件夹把内存撑爆。
const maxDeepFiles = 50000

type fileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // 相对共享文件夹的路径，用 / 分隔
	Dir   bool   `json:"dir,omitempty"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // 修改时间，毫秒
}

var errNoFile = errors.New("文件或文件夹不存在，可能已经被对方移走或删除")

// sharePath 检查同事请求的相对路径，返回给 os.Root 用的路径（根目录是 "."）。
// 跳出共享文件夹由 os.Root 挡住；这里再挡住隐藏文件和批注同步文件夹。
func sharePath(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if c == "" || c == "." || c == ".." || store.HiddenName(c) || (i == 0 && store.IsSyncDir(c)) ||
			// Windows 上 \ 也是分隔符、: 能指到文件的附加数据流，都不许出现在一段路径里
			(runtime.GOOS == "windows" && strings.ContainsAny(c, `\:`)) {
			return "", errNoFile
		}
	}
	return path.Join(parts...), nil
}

func (a *App) fileShareOn() bool {
	c := a.config()
	return c.ShareDir != "" && !c.NoFileShare
}

// openShareRoot 打开共享文件夹，之后的访问都不会跳出这个文件夹（包括符号链接）。
// 打不开时把原因写给对方：关闭了文件下载是 403，同事那边整个下载停下；共享文件夹打不开是 503，同事那边过一会儿重试。
func (a *App) openShareRoot(w http.ResponseWriter) *os.Root {
	if !a.fileShareOn() {
		http.Error(w, "对方关闭了文件下载", http.StatusForbidden)
		return nil
	}
	root, err := os.OpenRoot(a.config().ShareDir)
	if err != nil {
		http.Error(w, "对方的共享文件夹暂时打不开", http.StatusServiceUnavailable)
		return nil
	}
	return root
}

func entryOf(rel string, info fs.FileInfo) fileEntry {
	e := fileEntry{Name: info.Name(), Path: rel, Dir: info.IsDir(), MTime: info.ModTime().UnixMilli()}
	if !e.Dir {
		e.Size = info.Size()
	}
	return e
}

// listShare 列出一层文件夹。符号链接不列出。
func listShare(root *os.Root, rel string) ([]fileEntry, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, errNoFile
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, errNoFile
	}
	out := []fileEntry{}
	for _, d := range entries {
		if store.HiddenName(d.Name()) || (rel == "." && store.IsSyncDir(d.Name())) || !(d.Type().IsRegular() || d.IsDir()) {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		out = append(out, entryOf(path.Join(rel, d.Name()), info))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// walkShare 把一个文件夹展开成里面所有文件的清单；rel 是文件时就只有它自己。
func walkShare(root *os.Root, rel string) ([]fileEntry, error) {
	info, err := root.Stat(rel)
	if err != nil {
		return nil, errNoFile
	}
	if !info.IsDir() {
		return []fileEntry{entryOf(rel, info)}, nil
	}
	out := []fileEntry{}
	err = fs.WalkDir(root.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的子文件夹跳过
		}
		if p != rel && (store.HiddenName(d.Name()) || store.IsSyncDir(p)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if len(out) == maxDeepFiles {
			return errors.New("这个文件夹里的文件超过 5 万个，请进去分开下载")
		}
		out = append(out, entryOf(p, info))
		return nil
	})
	return out, err
}

// peerFiles 列出共享文件夹里的一层；带 deep=1 时把选中的文件夹展开成文件清单，下载整个文件夹用。
func (a *App) peerFiles(w http.ResponseWriter, r *http.Request) {
	rel, err := sharePath(r.URL.Query().Get("path"))
	if err != nil {
		httpErr(w, err, http.StatusNotFound)
		return
	}
	root := a.openShareRoot(w)
	if root == nil {
		return
	}
	defer root.Close()
	var list []fileEntry
	if r.URL.Query().Get("deep") == "1" {
		list, err = walkShare(root, rel)
	} else {
		list, err = listShare(root, rel)
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errNoFile) {
			code = http.StatusNotFound
		}
		httpErr(w, err, code)
		return
	}
	writeJSON(w, list)
}

// peerFile 把一个文件交给同事。支持 Range，断了能从断点接着下；带 Last-Modified，
// 续传时对方用 If-Range 判断文件有没有被改过，改过就从头下。
func (a *App) peerFile(w http.ResponseWriter, r *http.Request) {
	rel, err := sharePath(r.URL.Query().Get("path"))
	if err != nil || rel == "." {
		httpErr(w, errNoFile, http.StatusNotFound)
		return
	}
	root := a.openShareRoot(w)
	if root == nil {
		return
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("同事要的文件 %s 打不开：%v", rel, err)
		}
		httpErr(w, errNoFile, http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		httpErr(w, errNoFile, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
