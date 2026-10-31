package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fyne.io/systray"
	"lanreview/internal/media"
	"lanreview/internal/platform"
	"lanreview/internal/store"
)

type Config struct {
	Nickname    string   `json:"nickname"`
	ShareDir    string   `json:"shareDir"`
	TeamKey     string   `json:"teamKey,omitempty"`
	ManualPeers []string `json:"manualPeers,omitempty"`
	SilentStart bool     `json:"silentStart,omitempty"` // 开机启动时不弹出浏览器
	Paused      bool     `json:"paused,omitempty"`      // 程序留在后台，但不分享、不同步
	NoFileShare bool     `json:"noFileShare,omitempty"` // 不让同事下载共享文件夹里的文件（默认开放）
	// 0.4.0 起共享文件夹分成三个子文件夹，里面的所有文件同事都能下载。从旧版本更新上来的人提示过一次就记下来；
	// 新设置的人在设置页上已经看到了。LayoutNote 是整理共享文件夹的结果（挪了几项），提示时一起说
	FileShareNotified bool   `json:"fileShareNotified,omitempty"`
	LayoutNote        string `json:"layoutNote,omitempty"`
}

type App struct {
	dataDir string
	port    int
	videos  *media.VideoLib
	peers   *Peers
	client  *http.Client // 访问其他电脑的小请求
	stream  *http.Client // 转发视频，不设总超时
	upd     *updater
	dl      *downloads // 从同事那里拿的文件，见 downloads.go

	mu       sync.Mutex
	id       *store.Identity
	cfg      Config
	store    *store.Store
	versions *media.Versions
	backups  *media.Backups // 从同事那里拿来的视频，见 internal/media/backups.go
	onChange func()         // 设置变了时通知托盘刷新，有托盘时才有
	shareErr string         // 共享文件夹打不开的原因
}

func newApp(dataDir string, port int) (*App, error) {
	id, err := store.LoadIdentity(dataDir)
	if err != nil {
		return nil, err
	}
	a := &App{
		dataDir: dataDir,
		port:    port,
		id:      id,
		videos:  media.NewVideoLib(),
		peers:   newPeers(),
		upd:     newUpdater(dataDir),
		dl:      loadDownloads(dataDir),
		client:  &http.Client{Timeout: 30 * time.Second},
		stream: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second,
		}},
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &a.cfg); err != nil {
			return nil, err
		}
	}
	if a.cfg.ShareDir != "" {
		if err := a.openShare(a.cfg.ShareDir, false); err != nil {
			log.Printf("打开共享文件夹失败：%v", err)
		}
	}
	return a, nil
}

// openShare 打开共享文件夹。先整理成三个子文件夹（见 internal/store/layout.go）：从旧版本更新上来的，
// 或者 move 为真时（用户换了共享文件夹并确认过），把原有的东西挪进"分享"。
func (a *App) openShare(dir string, move bool) error {
	layout, err := store.PrepareShare(dir, move)
	var st *store.Store
	if err == nil {
		st, err = store.Open(dir)
	}
	if err != nil {
		a.mu.Lock()
		a.store, a.versions, a.backups, a.shareErr = nil, nil, nil, shareErrorText(dir, err)
		a.mu.Unlock()
		return err
	}
	if note := layout.Note(); layout.Migrated && note != "" {
		c := a.config()
		c.LayoutNote = note
		if err := a.saveConfig(c); err != nil {
			log.Printf("保存整理结果失败：%v", err)
		}
	}
	vs := media.LoadVersions(filepath.Join(st.Dir(), "versions.json"))
	bk := media.LoadBackups(filepath.Join(st.Dir(), "backups.json"))
	a.mu.Lock()
	a.store, a.versions, a.backups, a.shareErr = st, vs, bk, ""
	a.mu.Unlock()
	a.rescan()
	return nil
}

// shareErrorText 把打不开共享文件夹的原因说成用户能照着做的话。
func shareErrorText(dir string, err error) string {
	if errors.Is(err, fs.ErrPermission) && runtime.GOOS == "darwin" {
		return "macOS 没有允许「局域网审片」访问共享文件夹 " + dir + "。" +
			"请打开 系统设置 → 隐私与安全性 → 文件与文件夹，找到「局域网审片」，打开对应的文件夹（比如\"桌面文件夹\"）；列表里没有的话，点下面的\"重试\"，系统会弹出询问。" +
			"程序更新后可能需要重新允许一次；共享文件夹放在桌面、文稿、下载以外的地方（比如个人文件夹下新建一个）就不会被询问。"
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "共享文件夹 " + dir + " 不存在了，可能被移动、删除，或者所在的移动硬盘没接上。请在\"设置\"里重新选择。"
	}
	return "打不开共享文件夹 " + dir + "：" + err.Error()
}

func (a *App) shareError() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shareErr
}

// rescan 重新扫描共享文件夹，并把新出现的文件归入版本组。
func (a *App) rescan() {
	dir := a.config().ShareDir
	if dir == "" {
		return
	}
	a.videos.Scan(dir)
	if vs, st := a.getVersions(), a.getStore(); vs != nil && st != nil {
		// 备份是同事的视频，不进自己的版本组
		own, _ := a.getBackups().Split(a.videos.List())
		vs.Update(own, st.AnnotatedFPs())
	}
}

// me 是这台电脑当前的身份，导入身份后会换成新的。
func (a *App) me() *store.Identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.id
}

func (a *App) config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) getStore() *store.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

func (a *App) getBackups() *media.Backups {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backups
}

func (a *App) getVersions() *media.Versions {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.versions
}

func (a *App) saveConfig(c Config) error {
	a.mu.Lock()
	a.cfg = c
	a.mu.Unlock()
	data, _ := json.MarshalIndent(c, "", "  ")
	err := store.WriteFileAtomic(filepath.Join(a.dataDir, "config.json"), data)
	a.changed()
	return err
}

func (a *App) setOnChange(f func()) {
	a.mu.Lock()
	a.onChange = f
	a.mu.Unlock()
}

func (a *App) changed() {
	a.mu.Lock()
	f := a.onChange
	a.mu.Unlock()
	if f != nil {
		go f()
	}
}

func (a *App) paused() bool { return a.config().Paused }

// setPaused 暂停或恢复服务。暂停时程序留在后台：不广播、不同步、不扫描，同事看到的是离线。
func (a *App) setPaused(on bool) error {
	c := a.config()
	c.Paused = on
	if err := a.saveConfig(c); err != nil {
		return err
	}
	platform.SetKeepAwake(!on)
	if !on {
		go a.rescan()
		a.upd.kick(updateKick{github: true}) // 暂停期间没有检查更新，恢复时查一次
	}
	log.Printf("服务%s", map[bool]string{true: "已暂停", false: "已恢复"}[on])
	return nil
}

func (a *App) setSilentStart(on bool) error {
	c := a.config()
	c.SilentStart = on
	return a.saveConfig(c)
}

// autostartArgs 是开机启动时用的参数：不转到后台（由系统直接管理），并带上当前的端口和配置目录。
func (a *App) autostartArgs() []string {
	return []string{"-child", "-autostart", "-port", fmt.Sprint(a.port), "-data", a.dataDir}
}

func (a *App) setAutostart(on bool) error {
	err := platform.SetAutostart(on, a.autostartArgs())
	a.changed()
	return err
}

// hasTray 判断有没有显示托盘图标。
func (a *App) hasTray() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.onChange != nil
}

// quit 退出程序：有托盘时让托盘收尾，否则直接退出。
func (a *App) quit() {
	if a.hasTray() {
		systray.Quit()
		return
	}
	os.Exit(0)
}

func (a *App) setup(nickname, shareDir, teamKey string) error {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" || len([]rune(nickname)) > 20 {
		return errors.New("昵称不能为空，最多 20 个字")
	}
	st, err := os.Stat(shareDir)
	if err != nil || !st.IsDir() {
		return errors.New("共享文件夹不存在")
	}
	c := a.config()
	changed := c.ShareDir != shareDir
	c.Nickname, c.ShareDir, c.TeamKey = nickname, shareDir, strings.TrimSpace(teamKey)
	c.FileShareNotified = true // 设置页上写着共享文件夹里的文件同事都能下载
	if err := a.saveConfig(c); err != nil {
		return err
	}
	if changed || a.getStore() == nil {
		return a.openShare(shareDir, changed) // 换了文件夹时，网页上已经确认过把原有的东西挪进"分享"
	}
	return nil
}

// importIdentity 换成另一台电脑导出的身份。旧身份文件留一份备份。
func (a *App) importIdentity(f store.IdentityFile) (*store.Identity, error) {
	id, seed, err := f.Parse()
	if err != nil {
		return nil, err
	}
	if id.ID == a.me().ID {
		return id, nil
	}
	path := filepath.Join(a.dataDir, "identity.key")
	if old, err := os.ReadFile(path); err == nil {
		os.WriteFile(path+".bak-"+time.Now().Format("20060102-150405"), old, 0o600)
	}
	if err := store.WriteIdentity(path, seed); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.id = id
	if a.cfg.Nickname == "" && f.Nickname != "" {
		a.cfg.Nickname = f.Nickname
	}
	c := a.cfg
	a.mu.Unlock()
	return id, a.saveConfig(c)
}

// Windows 文件名不允许的字符
var reBadFileChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

// renameVideo 修改视频名称；renameFiles 为真时，同时把本机的文件改名为"名称_v版本号"。
// 指纹只看文件内容，改名后批注照样对得上。返回改名失败的文件说明。
func (a *App) renameVideo(group, name string, renameFiles bool) ([]string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 || reBadFileChars.MatchString(name) || strings.HasSuffix(name, ".") {
		return nil, errors.New(`名称不能为空，不超过 100 个字，不能包含 \ / : * ? " < > |，也不能以句点结尾`)
	}
	vs := a.getVersions()
	if vs == nil {
		return nil, errors.New("还没有设置共享文件夹")
	}
	members, err := vs.Members(group)
	if err != nil {
		return nil, err
	}
	var failed []string
	if renameFiles {
		for i, m := range members {
			v := a.videos.Get(m.FP)
			if v == nil {
				continue // 这个版本的文件已经不在了
			}
			target := filepath.Join(filepath.Dir(v.Path()), fmt.Sprintf("%s_v%02d%s", name, i+1, filepath.Ext(v.Path())))
			if target == v.Path() {
				continue
			}
			// 目标已存在时只允许是同一个文件（大小写不敏感的磁盘上只改大小写）
			if st, err := os.Stat(target); err == nil {
				if cur, err := os.Stat(v.Path()); err != nil || !os.SameFile(st, cur) {
					failed = append(failed, fmt.Sprintf("%s：同一个文件夹里已经有 %s", v.Name, filepath.Base(target)))
					continue
				}
			}
			if err := os.Rename(v.Path(), target); err != nil {
				failed = append(failed, fmt.Sprintf("%s：改名失败，文件可能正在被使用（比如同事正在播放），稍后再试", v.Name))
				log.Printf("改名失败 %s -> %s：%v", v.Path(), target, err)
			}
		}
	}
	if err := vs.SetName(group, name); err != nil {
		return nil, err
	}
	a.rescan()
	return failed, nil
}

func (a *App) start() {
	go a.discoveryLoop()
	go a.syncLoop()
	go a.manualLoop()
	go a.subnetScanLoop()
	go a.updateLoop()
	go a.downloadLoop()
	go func() {
		for i := 0; ; i++ {
			time.Sleep(5 * time.Second)
			// 共享文件夹打不开时（比如还没允许访问桌面）每 15 秒重试，允许后不用重启就能恢复
			if dir := a.config().ShareDir; dir != "" && a.getStore() == nil && i%3 == 0 {
				if a.openShare(dir, false) == nil {
					log.Printf("共享文件夹已恢复：%s", dir)
				}
			}
			if a.paused() {
				continue
			}
			a.rescan()
			// 清理被删批注的图片要把图片文件夹整个读一遍，一分钟一次就够了
			if st := a.getStore(); st != nil && i%12 == 0 {
				st.GC()
			}
		}
	}()
}

// lanAddrs 是本机在局域网里的地址，告诉同事用。
func (a *App) lanAddrs() []string {
	var out []string
	for _, ifi := range lanInterfaces() {
		for _, ip := range lanIPv4(ifi) {
			out = append(out, ip.String())
		}
	}
	return out
}
