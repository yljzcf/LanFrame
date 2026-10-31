package update

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Cache 是配置目录下的 updates 文件夹：updates/<版本>/ 里放验过签名的 update.json 和校验过的安装包。
// 只保留一个版本，就是知道的最新版本。这台电脑更新以后，同事可以从这里下载。
type Cache struct {
	dir string

	mu  sync.Mutex
	raw []byte // update.json 原文，原样转给同事，他们自己验签
	m   *Manifest
}

// OpenCache 打开缓存，删掉比正在运行的版本旧的和没用的版本。
func OpenCache(dir, running string) *Cache {
	c := &Cache{dir: dir}
	entries, _ := os.ReadDir(dir)
	keep := ""
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "update.json"))
		if err != nil {
			continue
		}
		m, err := Parse(raw)
		if err != nil || m.Version != e.Name() || Newer(running, m.Version) {
			continue
		}
		if c.m == nil || Newer(m.Version, c.m.Version) {
			c.raw, c.m, keep = raw, m, e.Name()
		}
	}
	for _, e := range entries {
		if e.Name() != keep {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return c
}

// Manifest 返回缓存的清单和它的原文，没有时都是 nil。
func (c *Cache) Manifest() ([]byte, *Manifest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw, c.m
}

// Version 是缓存的版本，没有时为空。
func (c *Cache) Version() string {
	if _, m := c.Manifest(); m != nil {
		return m.Version
	}
	return ""
}

// Put 换成新版本的清单，旧版本的文件夹删掉。清单必须是 Parse 过的。
func (c *Cache) Put(raw []byte, m *Manifest) error {
	dir := filepath.Join(c.dir, m.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "update.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "update.json")); err != nil {
		return err
	}
	c.mu.Lock()
	old := c.m
	c.raw, c.m = raw, m
	c.mu.Unlock()
	if old != nil && old.Version != m.Version {
		os.RemoveAll(filepath.Join(c.dir, old.Version))
	}
	return nil
}

// Path 返回某个系统的安装包在缓存里的路径；还没下载好时 ok 为 false。
func (c *Cache) Path(goos string) (path string, f File, ok bool) {
	_, m := c.Manifest()
	if m == nil {
		return "", File{}, false
	}
	f, ok = m.Files[goos]
	if !ok {
		return "", File{}, false
	}
	path = filepath.Join(c.dir, m.Version, f.Name)
	return path, f, downloaded(path, f)
}

// Complete 返回安装包全都下载好的缓存版本；还有没下完的，或者没有缓存时为空。
func (c *Cache) Complete() string {
	_, m := c.Manifest()
	if m == nil {
		return ""
	}
	for _, f := range m.Files {
		if !downloaded(filepath.Join(c.dir, m.Version, f.Name), f) {
			return ""
		}
	}
	return m.Version
}

func downloaded(path string, f File) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() == f.Size
}

// Files 返回已经下载好的安装包：文件名 -> 路径。
func (c *Cache) Files() map[string]string {
	out := map[string]string{}
	_, m := c.Manifest()
	if m == nil {
		return out
	}
	for goos := range m.Files {
		if path, f, ok := c.Path(goos); ok {
			out[f.Name] = path
		}
	}
	return out
}

// Download 从 url 下载某个系统的安装包到缓存。
func (c *Cache) Download(ctx context.Context, client *http.Client, url, goos string) error {
	path, f, ok := c.Path(goos)
	if ok {
		return nil
	}
	if f.Name == "" {
		return errors.New("更新清单里没有这个系统的安装包")
	}
	return Download(ctx, client, url, f, path)
}
