package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"lanreview/internal/platform"
	"lanreview/internal/update"
)

// 自动更新：在后台检查、下载新版本，下载好后由用户点"立即更新"（网页或托盘）换上新版本。
// 新版本先找局域网里的同事要（他们更新过的话，安装包留在自己的缓存里），也查 GitHub。
// 不管从哪里拿到的，都要用内置公钥验过签名、对过哈希，只接受比自己新的版本。

const (
	updateInterval = 6 * time.Hour    // 多久查一次 GitHub
	updateRetry    = 10 * time.Minute // 查 GitHub 失败、或者某个来源下载失败后，多久再试
)

// updateKick 是一次检查的要求。
type updateKick struct {
	github bool // 也查 GitHub（不然只看同事那里有没有新版本）
	manual bool // 用户点的：暂停服务时也查，不管之前的失败
}

type updater struct {
	cache  *update.Cache
	client *http.Client
	signal chan struct{}

	mu       sync.Mutex
	pending  updateKick
	busy     string               // 正在做什么，界面上显示
	err      string               // 上次检查或下载失败的原因
	blocked  string               // 新版本下载好了，但不能原地替换的原因
	checked  time.Time            // 上次查 GitHub 的时间
	next     time.Time            // 下次查 GitHub 的时间
	failed   map[string]time.Time // 来源（同事地址或 github）-> 上次失败的时间
	applying bool
}

func newUpdater(dataDir string) *updater {
	return &updater{
		cache: update.OpenCache(filepath.Join(dataDir, "updates"), version),
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		}},
		signal: make(chan struct{}, 1),
		failed: map[string]time.Time{},
	}
}

// kick 要求检查一次。已经有一次在排队时合并成一次。
func (u *updater) kick(k updateKick) {
	u.mu.Lock()
	u.pending.github = u.pending.github || k.github
	u.pending.manual = u.pending.manual || k.manual
	u.mu.Unlock()
	select {
	case u.signal <- struct{}{}:
	default:
	}
}

// known 是知道的最新版本：正在运行的，或者缓存里更新的。
func (u *updater) known() string {
	if v := u.cache.Version(); update.Newer(v, version) {
		return v
	}
	return version
}

// ready 判断新版本的安装包是否已经下载好，可以换上。
func (u *updater) ready() (string, bool) {
	v := u.cache.Version()
	_, _, ok := u.cache.Path(runtime.GOOS)
	return v, ok && update.Newer(v, version)
}

func (u *updater) setBusy(s string) {
	u.mu.Lock()
	u.busy = s
	u.mu.Unlock()
}

func (u *updater) setErr(s string) {
	u.mu.Lock()
	u.err = s
	u.mu.Unlock()
}

// skip 判断某个来源最近是否失败过。用户手动点的检查不管这个。
func (u *updater) skip(src string, manual bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return !manual && time.Since(u.failed[src]) < updateRetry
}

// fail 记下某个来源失败了，10 分钟内不再找它。name 是写日志用的名字。
func (u *updater) fail(src, name string, err error) {
	u.mu.Lock()
	u.failed[src] = time.Now()
	u.mu.Unlock()
	log.Printf("更新：从 %s 获取失败：%v", name, err)
}

// updateState 是给网页和托盘看的更新状态。
type updateState struct {
	Supported bool   `json:"supported"`          // 开发版不检查更新
	Latest    string `json:"latest,omitempty"`   // 比当前新的版本
	Ready     bool   `json:"ready"`              // 新版本已经下载好
	Busy      string `json:"busy,omitempty"`     // 正在做什么
	Error     string `json:"error,omitempty"`    // 上次失败的原因
	Blocked   string `json:"blocked,omitempty"`  // 不能原地替换的原因
	Download  string `json:"download,omitempty"` // 不能原地替换时，手动下载新版本的地址
	Checked   int64  `json:"checked,omitempty"`  // 上次查 GitHub 的时间
	Applying  bool   `json:"applying,omitempty"` // 正在换新版本、重启
}

func (a *App) updateState() updateState {
	u := a.upd
	latest, ready := u.ready()
	u.mu.Lock()
	defer u.mu.Unlock()
	s := updateState{
		Supported: update.Valid(version), Ready: ready, Busy: u.busy, Error: u.err,
		Applying: u.applying,
	}
	if update.Newer(latest, version) {
		s.Latest = latest
	}
	if !u.checked.IsZero() {
		s.Checked = u.checked.UnixMilli()
	}
	if ready && u.blocked != "" {
		s.Blocked = u.blocked
		if _, f, ok := u.cache.Path(runtime.GOOS); ok {
			s.Download = "/download/" + f.Name
		}
	}
	return s
}

// updateLoop 处理检查要求，并且每隔一段时间查一次 GitHub。开发版不检查。
func (a *App) updateLoop() {
	if !update.Valid(version) {
		return
	}
	// 等程序稳定下来、开机时网络连上，再清理上次更新留下的文件、开始检查
	time.Sleep(5 * time.Second)
	update.Cleanup()
	a.upd.kick(updateKick{github: true})
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-a.upd.signal:
		case <-tick.C:
			a.upd.mu.Lock()
			due := time.Now().After(a.upd.next)
			a.upd.mu.Unlock()
			_, _, ok := a.upd.cache.Path(runtime.GOOS)
			switch {
			case due:
				a.upd.kick(updateKick{github: true})
			case update.Newer(a.upd.cache.Version(), version) && !ok:
				a.upd.kick(updateKick{}) // 新版本还没下载完，接着下
			}
			continue
		}
		a.upd.mu.Lock()
		k := a.upd.pending
		a.upd.pending = updateKick{}
		a.upd.mu.Unlock()
		a.checkUpdate(k)
	}
}

// checkUpdate 找新版本的清单，然后下载安装包。只在 updateLoop 里调用，不会同时有两个在跑。
func (a *App) checkUpdate(k updateKick) {
	u := a.upd
	if a.paused() && !k.manual {
		return // 暂停服务时不联网，恢复服务时会再检查
	}
	wasReady := false
	if _, ok := u.ready(); ok {
		wasReady = true
	}
	defer func() {
		u.setBusy("")
		v, ok := u.ready()
		blocked := ""
		if ok {
			if err := update.CanInstall(); err != nil {
				blocked = err.Error()
			}
		}
		u.mu.Lock()
		u.blocked = blocked
		u.mu.Unlock()
		if ok && !wasReady {
			log.Printf("更新：新版本 %s 已经下载好，等用户点更新", v)
			a.changed() // 托盘显示"更新到"
		}
	}()

	// 1. 同事那里有更新的版本：从缓存版本最新的那位开始问
	for _, p := range a.updatePeers("") {
		if !update.Newer(p.Update, u.known()) {
			break
		}
		if u.skip(p.Addr, k.manual) {
			continue
		}
		u.setBusy("正在从 " + p.Name + " 获取新版本信息")
		if err := a.fetchManifest("http://"+p.Addr+"/peer/update", p.Name); err != nil {
			u.fail(p.Addr, p.Name, err)
		}
	}
	// 2. GitHub
	if k.github {
		u.setBusy("正在检查新版本")
		err := a.fetchManifest(update.LatestURL(), "GitHub")
		now := time.Now()
		u.mu.Lock()
		u.checked = now
		if err != nil {
			u.next = now.Add(updateRetry)
			var ne net.Error
			if errors.As(err, &ne) {
				u.err = "连不上 GitHub，没能检查新版本；局域网里的同事有新版本时，仍会自动从他那里获取"
			} else {
				u.err = "检查新版本失败：" + err.Error()
			}
		} else {
			u.next = now.Add(updateInterval)
			u.err = ""
		}
		u.mu.Unlock()
		if err != nil {
			log.Printf("更新：检查 GitHub 失败：%v", err)
		}
	}
	// 3. 下载安装包
	a.fetchPackages(k.manual)
}

// updatePeers 是在线、缓存里有新版本的同事，缓存版本新的在前。exact 不为空时只要缓存的正好是这个版本的。
func (a *App) updatePeers(exact string) []Peer {
	var out []Peer
	for _, p := range a.peers.Snapshot() {
		if p.Online && p.ID != a.me().ID && update.Newer(p.Update, version) && (exact == "" || p.Update == exact) {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(x, y Peer) int {
		switch {
		case update.Newer(x.Update, y.Update):
			return -1
		case update.Newer(y.Update, x.Update):
			return 1
		}
		return 0
	})
	return out
}

// fetchManifest 读取一份更新清单，验过签名、比知道的版本新，就放进缓存。
func (a *App) fetchManifest(url, from string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := update.Get(ctx, a.upd.client, url, 64<<10)
	if err != nil {
		return err
	}
	m, err := update.Parse(raw)
	if err != nil {
		return err
	}
	if !update.Newer(m.Version, a.upd.known()) {
		return nil
	}
	if err := a.upd.cache.Put(raw, m); err != nil {
		return err
	}
	log.Printf("更新：从 %s 得知新版本 %s", from, m.Version)
	return nil
}

// fetchPackages 下载缓存版本的安装包：先下本机系统的，再下另一个系统的，这样也能给用另一个系统的同事中转。
// 来源先找缓存了同一个版本的同事，再找 GitHub。
func (a *App) fetchPackages(manual bool) {
	u := a.upd
	_, m := u.cache.Manifest()
	if m == nil {
		return
	}
	// 已经是这个版本了：只有之前就在中转的，才把缺的另一个系统的安装包补上
	if !update.Newer(m.Version, version) && len(u.cache.Files()) == 0 {
		return
	}
	systems := []string{runtime.GOOS}
	for goos := range m.Files {
		if goos != runtime.GOOS {
			systems = append(systems, goos)
		}
	}
	for _, goos := range systems {
		f, ok := m.Files[goos]
		if _, _, have := u.cache.Path(goos); !ok || have {
			continue
		}
		own := goos == runtime.GOOS
		var lastErr error
		done := false
		for _, src := range a.packageSources(m.Version, f.Name) {
			if u.skip(src.key, manual) {
				continue
			}
			if own {
				u.setBusy(fmt.Sprintf("正在从 %s 下载新版本 %s", src.name, m.Version))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			err := u.cache.Download(ctx, u.client, src.url, goos)
			cancel()
			if err == nil {
				log.Printf("更新：已从 %s 下载 %s %s", src.name, m.Version, f.Name)
				done = true
				break
			}
			lastErr = err
			u.fail(src.key, src.name, err)
		}
		if own && !done {
			if lastErr != nil {
				u.setErr(fmt.Sprintf("新版本 %s 下载失败：%v", m.Version, lastErr))
			}
			return
		}
		if own {
			u.setErr("")
		}
	}
}

type packageSource struct{ key, name, url string }

func (a *App) packageSources(ver, name string) []packageSource {
	var out []packageSource
	for _, p := range a.updatePeers(ver) {
		out = append(out, packageSource{p.Addr, p.Name, "http://" + p.Addr + "/peer/update/" + name})
	}
	return append(out, packageSource{"github", "GitHub", update.FileURL(ver, name)})
}

// applyUpdate 换上已经下载好的新版本，启动新程序，然后退出。
func (a *App) applyUpdate() error {
	u := a.upd
	_, m := u.cache.Manifest()
	path, f, ok := u.cache.Path(runtime.GOOS)
	if m == nil || !ok || !update.Newer(m.Version, version) {
		return errors.New("新版本还没下载好")
	}
	u.mu.Lock()
	if u.applying {
		u.mu.Unlock()
		return errors.New("正在更新")
	}
	u.applying = true
	u.mu.Unlock()
	fail := func(err error) error {
		u.mu.Lock()
		u.applying = false
		u.mu.Unlock()
		log.Printf("更新到 %s 失败：%v", m.Version, err)
		return err
	}
	if err := update.VerifyFile(path, f); err != nil {
		return fail(err)
	}
	exe, err := update.Install(path)
	if err != nil {
		return fail(err)
	}
	// 新程序会等这个进程退出、让出端口后再启动服务；不再打开浏览器，已经打开的网页会自己刷新
	args := []string{"-child", "-restart", "-no-browser", "-port", fmt.Sprint(a.port), "-data", a.dataDir}
	if !a.hasTray() {
		args = append(args, "-no-tray")
	}
	if err := update.StartNew(exe, args); err != nil {
		return fail(fmt.Errorf("新版本已经装好，但没能自动重启，请退出程序后重新打开：%v", err))
	}
	log.Printf("已换上新版本 %s，重启中", m.Version)
	go func() {
		time.Sleep(300 * time.Millisecond) // 先让网页收到回应
		a.quit()
	}()
	return nil
}

// applyUpdateFromTray 是托盘里点"更新到"：失败时弹框说明，托盘没有别的地方显示错误。
func (a *App) applyUpdateFromTray() {
	if err := a.applyUpdate(); err != nil {
		platform.ShowError("局域网审片更新失败", err.Error())
	}
}

// ---- 局域网接口：把缓存里的新版本给同事 ----

func (a *App) peerUpdate(w http.ResponseWriter, r *http.Request) {
	raw, _ := a.upd.cache.Manifest()
	if raw == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}

func (a *App) peerUpdateFile(w http.ResponseWriter, r *http.Request) {
	path, ok := a.upd.cache.Files()[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// ---- 本机接口 ----

func (a *App) apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !update.Valid(version) {
		httpErr(w, errors.New("开发版不检查更新"), http.StatusBadRequest)
		return
	}
	a.upd.setBusy("正在检查新版本")
	a.upd.kick(updateKick{github: true, manual: true})
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if err := a.applyUpdate(); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
