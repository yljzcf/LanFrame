package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lanreview/internal/media"
	"lanreview/internal/platform"
	"lanreview/internal/store"
)

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	// 给局域网里其他电脑用的接口：只开放共享文件夹里扫描到的视频和批注同步文件夹
	// 设了团队口令时，除了 /peer/info 都要带上口令
	peer := http.NewServeMux()
	peer.HandleFunc("GET /peer/info", a.peerInfo)
	peer.HandleFunc("GET /peer/videos", a.teamOnly(a.peerVideos))
	peer.HandleFunc("GET /peer/video/{fp}", a.teamOnly(a.peerVideo))
	peer.HandleFunc("GET /peer/records", a.teamOnly(a.peerRecords))
	peer.HandleFunc("GET /peer/record/{id}", a.teamOnly(a.peerRecord))
	peer.HandleFunc("GET /peer/image/{name}", a.teamOnly(a.peerImage))
	// 新版本的安装包本来就是公开的，靠签名防伪，不需要团队口令
	peer.HandleFunc("GET /peer/update", a.peerUpdate)
	peer.HandleFunc("GET /peer/update/{name}", a.peerUpdateFile)
	mux.Handle("/peer/", notFromBrowser(peer))
	mux.HandleFunc("GET /download/{name}", a.download)

	// 只给本机网页用的接口
	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", a.apiState)
	api.HandleFunc("POST /api/setup", a.apiSetup)
	api.HandleFunc("GET /api/fs", a.apiFS)
	api.HandleFunc("POST /api/peers", a.apiAddPeer)
	api.HandleFunc("POST /api/rescan", a.apiRescan)
	api.HandleFunc("GET /api/stream/{fp}", a.apiStream)
	api.HandleFunc("GET /api/annotations", a.apiAnnotations)
	api.HandleFunc("POST /api/annotations", a.apiCreate)
	api.HandleFunc("POST /api/annotations/{ann}/{action}", a.apiAction)
	api.HandleFunc("POST /api/images", a.apiUploadImage)
	api.HandleFunc("GET /api/images/{name}", a.peerImage)
	api.HandleFunc("POST /api/versions/answer", a.apiVersionAnswer)
	api.HandleFunc("POST /api/versions/move", a.apiVersionMove)
	api.HandleFunc("POST /api/versions/rename", a.apiVersionRename)
	api.HandleFunc("GET /api/identity", a.apiExportIdentity)
	api.HandleFunc("POST /api/identity", a.apiImportIdentity)
	api.HandleFunc("POST /api/quit", a.apiQuit)
	api.HandleFunc("POST /api/toggle", a.apiToggle)
	api.HandleFunc("POST /api/update/check", a.apiUpdateCheck)
	api.HandleFunc("POST /api/update/apply", a.apiUpdateApply)
	// 只看来源地址挡不住跨站请求：用户浏览的任何网页都能让浏览器往 localhost 发 POST，
	// 所以再拒绝其他网站发来的修改请求
	mux.Handle("/api/", localOnly(http.NewCrossOriginProtection().Handler(api)))

	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web)))
	mux.HandleFunc("GET /favicon.png", favicon)
	mux.HandleFunc("GET /{$}", a.index)
	return a.pausedGate(mux)
}

// notFromBrowser：/peer/ 只给其他电脑上的审片程序用。浏览器发的请求都带 Sec-Fetch-Site，程序之间的请求不带，
// 这样恶意网页即使用 DNS 重绑定把自己的域名指向这台电脑，也读不到视频和批注。
func notFromBrowser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "只给审片程序之间使用", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var faviconPNG = sync.OnceValue(func() []byte { return platform.AppIcon(64, false) })

func favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(faviconPNG())
}

// pausedGate：暂停服务时只给本机网页用，局域网里的请求一律拒绝，同事那边看到的就是离线。
func (a *App) pausedGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.paused() && !isLocal(r) {
			http.Error(w, "服务已暂停", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLocal 判断请求是否来自本机浏览器。同时检查 Host，防止其他网站借道访问本机接口。
func isLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	h, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		h = r.Host
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocal(r) {
			http.Error(w, "只能在本机访问", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) teamOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := teamToken(a.config().TeamKey)
		if want != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Team-Key")), []byte(want)) != 1 {
			http.Error(w, errForbidden.Error(), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func httpErr(w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
}

// ---- 局域网接口 ----

func (a *App) peerInfo(w http.ResponseWriter, r *http.Request) {
	c := a.config()
	writeJSON(w, peerInfo{
		App: appName, ID: a.me().ID, Name: c.Nickname, Port: a.port, Locked: c.TeamKey != "",
		Version: version, Update: a.upd.cache.Complete(),
	})
}

// listing 是本机分享的视频列表，带上版本信息。
func (a *App) listing() []*media.VideoEntry {
	vs, st := a.getVersions(), a.getStore()
	if vs == nil || st == nil {
		return []*media.VideoEntry{}
	}
	return vs.Listing(a.videos.List(), st.AnnotatedFPs())
}

func (a *App) peerVideos(w http.ResponseWriter, r *http.Request) {
	list := a.listing()
	for _, v := range list {
		v.Suggest = ""
	}
	writeJSON(w, list)
}

func (a *App) peerVideo(w http.ResponseWriter, r *http.Request) {
	v := a.videos.Get(r.PathValue("fp"))
	if v == nil {
		http.NotFound(w, r)
		return
	}
	serveVideoFile(w, r, v)
}

func serveVideoFile(w http.ResponseWriter, r *http.Request, v *media.VideoInfo) {
	f, err := os.Open(v.Path())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	// MOV 里的 H.264 浏览器也能放，统一按 mp4 告诉浏览器
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, v.Name, time.UnixMilli(v.ModTime), f)
}

func (a *App) peerRecords(w http.ResponseWriter, r *http.Request) {
	st := a.getStore()
	if st == nil {
		writeJSON(w, []store.RecordStub{})
		return
	}
	writeJSON(w, st.List())
}

func (a *App) peerRecord(w http.ResponseWriter, r *http.Request) {
	st, id := a.getStore(), r.PathValue("id")
	if st == nil || !store.ValidRecordID(id) || !st.Has(id) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	http.ServeFile(w, r, st.RecordPath(id))
}

func (a *App) peerImage(w http.ResponseWriter, r *http.Request) {
	st, name := a.getStore(), r.PathValue("name")
	if st == nil || !store.ValidImageName(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "max-age=31536000, immutable")
	http.ServeFile(w, r, st.ImagePath(name))
}

// downloadDir 是放安装包的文件夹：程序所在的文件夹；Mac 上是 .app 所在的文件夹。
func downloadDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	if strings.HasSuffix(dir, ".app/Contents/MacOS") {
		dir = filepath.Dir(filepath.Dir(filepath.Dir(dir)))
	}
	return dir
}

type downloadFile struct{ Name, Path string }

// downloadFiles 列出安装包给同事下载：自动更新缓存里的（最新，验过签名）优先，再加上放在程序旁边的。
// Mac 版要用 zip，浏览器下载的 .app 或裸程序会丢掉可执行权限。
func (a *App) downloadFiles() []downloadFile {
	var out []downloadFile
	cached := a.upd.cache.Files()
	for name, path := range cached {
		out = append(out, downloadFile{name, path})
	}
	if dir := downloadDir(); dir != "" {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			n := e.Name()
			if _, dup := cached[n]; !dup && e.Type().IsRegular() && strings.HasPrefix(n, appName) && (strings.HasSuffix(n, ".exe") || strings.HasSuffix(n, ".zip")) {
				out = append(out, downloadFile{n, filepath.Join(dir, n)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *App) download(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, f := range a.downloadFiles() {
		if f.Name == name {
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			http.ServeFile(w, r, f.Path)
			return
		}
	}
	http.NotFound(w, r)
}

var guideTmpl = template.Must(template.ParseFS(webFS, "web/guide.html"))

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	if isLocal(r) {
		data, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}
	// 同事用浏览器打开这台电脑的地址时，看到的是下载和加入引导
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	guideTmpl.Execute(w, map[string]any{
		"Name":  a.config().Nickname,
		"Files": a.downloadFiles(),
	})
}

// ---- 本机接口 ----

func (a *App) apiState(w http.ResponseWriter, r *http.Request) {
	c := a.config()
	st := a.getStore()
	me := a.me()
	peers := []Peer{}
	for _, p := range a.peers.Snapshot() {
		// 导入身份后，旧电脑如果还开着会用同一个身份出现，不当成同事显示
		if p.ID != me.ID {
			peers = append(peers, p)
		}
	}
	resp := map[string]any{
		// 设置过但共享文件夹暂时打不开时，仍算设置过，界面上提示原因，不回到欢迎页
		"configured": c.Nickname != "" && c.ShareDir != "",
		"shareError": a.shareError(),
		"me":         map[string]string{"id": me.ID, "code": store.DeviceCode(me.ID), "name": c.Nickname},
		"shareDir":   c.ShareDir,
		"teamKey":    c.TeamKey,
		"paused":     c.Paused,
		"toggles": map[string]bool{
			"autostart":          platform.AutostartEnabled(),
			"autostartSupported": platform.AutostartSupported(),
			"silentStart":        c.SilentStart,
		},
		"port":    a.port,
		"addrs":   a.lanAddrs(),
		"mine":    a.listing(),
		"peers":   peers,
		"os":      runtime.GOOS,
		"version": version,
		"update":  a.updateState(),
	}
	if st != nil {
		resp["syncDir"] = st.Dir()
		resp["annVersion"] = st.Version()
		resp["counts"] = st.AnnCounts()
	}
	writeJSON(w, resp)
}

func (a *App) apiSetup(w http.ResponseWriter, r *http.Request) {
	var body struct{ Nickname, ShareDir, TeamKey string }
	if err := readJSON(r, &body); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	if err := a.setup(body.Nickname, body.ShareDir, body.TeamKey); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiFS 给网页里的文件夹选择器用：浏览器拿不到本地路径，所以由程序列出目录。
func (a *App) apiFS(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	p := r.URL.Query().Get("path")
	if p == "" {
		p = home
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		httpErr(w, errors.New("打不开这个文件夹"), http.StatusBadRequest)
		return
	}
	type item struct {
		Name  string `json:"name,omitempty"`
		Label string `json:"label,omitempty"`
		Path  string `json:"path"`
	}
	dirs := []item{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "$") && e.Name() != store.SyncDirName {
			dirs = append(dirs, item{Name: e.Name(), Path: filepath.Join(p, e.Name())})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	roots := []item{{Label: "个人文件夹", Path: home}, {Label: "桌面", Path: filepath.Join(home, "Desktop")}}
	if runtime.GOOS == "windows" {
		for c := 'C'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			if _, err := os.Stat(d); err == nil {
				roots = append(roots, item{Label: string(c) + " 盘", Path: d})
			}
		}
	} else if vols, err := os.ReadDir("/Volumes"); err == nil {
		for _, v := range vols {
			roots = append(roots, item{Label: v.Name(), Path: filepath.Join("/Volumes", v.Name())})
		}
	}
	writeJSON(w, map[string]any{"path": p, "parent": parent, "dirs": dirs, "roots": roots})
}

func (a *App) apiAddPeer(w http.ResponseWriter, r *http.Request) {
	var body struct{ Addr string }
	if err := readJSON(r, &body); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	info, err := a.addManual(body.Addr)
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, info)
}

func (a *App) apiRescan(w http.ResponseWriter, r *http.Request) {
	// 共享文件夹没打开时先重新打开（用户点"重试"会走这里，macOS 也会在这时弹出访问询问）
	if dir := a.config().ShareDir; dir != "" && a.getStore() == nil {
		if err := a.openShare(dir); err != nil {
			httpErr(w, errors.New(a.shareError()), http.StatusBadRequest)
			return
		}
	}
	a.rescan()
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiVersionAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FP  string
		Yes bool
	}
	vs := a.getVersions()
	if err := readJSON(r, &body); err != nil || vs == nil {
		httpErr(w, errors.New("请求不对"), http.StatusBadRequest)
		return
	}
	if err := vs.Answer(body.FP, body.Yes); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiVersionMove(w http.ResponseWriter, r *http.Request) {
	var body struct{ FP, Group string }
	vs := a.getVersions()
	if err := readJSON(r, &body); err != nil || vs == nil {
		httpErr(w, errors.New("请求不对"), http.StatusBadRequest)
		return
	}
	if err := vs.Move(body.FP, body.Group); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiVersionRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group, Name string
		RenameFiles bool
	}
	if err := readJSON(r, &body); err != nil {
		httpErr(w, errors.New("请求不对"), http.StatusBadRequest)
		return
	}
	failed, err := a.renameVideo(body.Group, body.Name, body.RenameFiles)
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"failed": failed})
}

func (a *App) apiExportIdentity(w http.ResponseWriter, r *http.Request) {
	me, c := a.me(), a.config()
	name := fmt.Sprintf("审片身份-%s-%s.json", c.Nickname, store.DeviceCode(me.ID))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.MarshalIndent(me.Export(c.Nickname), "", "  ")
	w.Write(data)
}

func (a *App) apiImportIdentity(w http.ResponseWriter, r *http.Request) {
	var f store.IdentityFile
	if err := readJSON(r, &f); err != nil {
		httpErr(w, errors.New("不是有效的身份文件"), http.StatusBadRequest)
		return
	}
	id, err := a.importIdentity(f)
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"code": store.DeviceCode(id.ID)})
}

// apiQuit 从网页退出程序。
func (a *App) apiQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		log.Printf("从网页退出")
		a.quit()
	}()
}

// apiToggle 改托盘里也有的三个开关：运行服务、开机启动、开机后静默运行。
func (a *App) apiToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string
		On   bool
	}
	if err := readJSON(r, &body); err != nil {
		httpErr(w, errors.New("请求不对"), http.StatusBadRequest)
		return
	}
	var err error
	switch body.Name {
	case "running":
		err = a.setPaused(!body.On)
	case "autostart":
		err = a.setAutostart(body.On)
	case "silentStart":
		err = a.setSilentStart(body.On)
	default:
		err = errors.New("未知的开关")
	}
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiStream 把视频交给本机浏览器：本机有就直接读，没有就转发对方电脑上的原文件。
// 走本机地址可以让网页正常截取画面。
func (a *App) apiStream(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fp")
	if v := a.videos.Get(fp); v != nil {
		serveVideoFile(w, r, v)
		return
	}
	if a.paused() {
		http.Error(w, "服务已暂停", http.StatusServiceUnavailable)
		return
	}
	owner := r.URL.Query().Get("owner")
	var candidates []Peer
	for _, p := range a.peers.Snapshot() {
		if !p.Online {
			continue
		}
		for _, v := range p.Videos {
			if v.FP == fp && !v.Missing {
				if p.ID == owner {
					candidates = append([]Peer{p}, candidates...)
				} else {
					candidates = append(candidates, p)
				}
				break
			}
		}
	}
	for _, p := range candidates {
		req, err := a.peerRequest(r.Context(), "http://"+p.Addr+"/peer/video/"+fp)
		if err != nil {
			continue
		}
		for _, h := range []string{"Range", "If-Range"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := a.stream.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "Etag"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	http.Error(w, "视频所在的电脑不在线", http.StatusNotFound)
}

func (a *App) apiAnnotations(w http.ResponseWriter, r *http.Request) {
	st := a.getStore()
	if st == nil {
		httpErr(w, errors.New("还没有设置共享文件夹"), http.StatusBadRequest)
		return
	}
	items, deleted := st.Annotations(r.URL.Query().Get("fp"), a.me().ID)
	writeJSON(w, map[string]any{"items": items, "deleted": deleted, "version": st.Version()})
}

func checkContent(st *store.Store, c *store.Content) error {
	if c.Frame < 0 || utf8.RuneCountInString(c.Text) > 5000 || len(c.Strokes) > 500 || len(c.Attachments) > 20 {
		return errors.New("批注内容不合法")
	}
	if strings.TrimSpace(c.Text) == "" && len(c.Strokes) == 0 && len(c.Attachments) == 0 {
		return errors.New("批注是空的：写点文字、画一笔或者加一张图")
	}
	for _, name := range append([]string{c.Shot}, c.Attachments...) {
		if name == "" {
			continue
		}
		if !store.ValidImageName(name) {
			return errors.New("图片名不对")
		}
		if _, err := os.Stat(st.ImagePath(name)); err != nil {
			return errors.New("图片还没上传")
		}
	}
	return nil
}

func (a *App) newRecord(st *store.Store, me *store.Identity, typ, ann string) *store.Record {
	return &store.Record{
		Type: typ, Ann: ann, Clock: st.NextClock(), Time: time.Now().UnixMilli(),
		Author: me.ID, AuthorName: a.config().Nickname,
	}
}

func (a *App) commit(w http.ResponseWriter, st *store.Store, me *store.Identity, rec *store.Record) {
	if err := me.Sign(rec); err != nil {
		httpErr(w, err, http.StatusInternalServerError)
		return
	}
	if _, err := st.Add(rec); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"id": rec.ID})
}

func (a *App) apiCreate(w http.ResponseWriter, r *http.Request) {
	st := a.getStore()
	if st == nil {
		httpErr(w, errors.New("还没有设置共享文件夹"), http.StatusBadRequest)
		return
	}
	var body struct {
		Video   store.VideoRef
		Content store.Content
		ReplyTo string
	}
	if err := readJSON(r, &body); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	var quote *store.Quote
	if body.ReplyTo != "" {
		// 回复：视频和时间码跟原批注一致，不带截图和画面标注，并带上原批注的内容
		parent := st.Get(body.ReplyTo)
		if parent == nil || parent.Type != "create" {
			httpErr(w, errors.New("要回复的批注不存在或已被删除"), http.StatusNotFound)
			return
		}
		if parent.ReplyTo != "" { // 回复的回复也挂在最上面那条批注下
			body.ReplyTo = parent.ReplyTo
		}
		items, _ := st.Annotations(parent.Video.FP, "")
		quote = &store.Quote{Author: parent.Author, AuthorName: parent.AuthorName, Text: parent.Content.Text}
		for _, it := range items {
			if it.ID == parent.ID {
				quote.Text = it.Content.Text // 用原批注最新修改后的内容
			}
		}
		body.Video = *parent.Video
		body.Content.Frame, body.Content.Seconds = parent.Content.Frame, parent.Content.Seconds
		body.Content.Shot, body.Content.Strokes = "", nil
	}
	if !media.ValidFP(body.Video.FP) || body.Video.Owner == "" {
		httpErr(w, errors.New("视频信息不完整"), http.StatusBadRequest)
		return
	}
	if err := checkContent(st, &body.Content); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	// 批注 ID 前半段是作者标记，后半段随机
	me := a.me()
	id := store.AuthorTag(me.ID) + store.RandomHex(8)
	rec := a.newRecord(st, me, "create", id)
	rec.ID = id
	rec.Video = &body.Video
	rec.Content = &body.Content
	rec.ReplyTo = body.ReplyTo
	rec.Quote = quote
	a.commit(w, st, me, rec)
}

func (a *App) apiAction(w http.ResponseWriter, r *http.Request) {
	st := a.getStore()
	ann, action := r.PathValue("ann"), r.PathValue("action")
	if st == nil || !store.ValidRecordID(ann) {
		http.NotFound(w, r)
		return
	}
	create := st.Get(ann)
	if create == nil || create.Type != "create" {
		httpErr(w, errors.New("批注不存在或已被删除"), http.StatusNotFound)
		return
	}
	me := a.me()
	isAuthor := create.Author == me.ID
	rec := a.newRecord(st, me, action, ann)
	rec.ID = store.RandomHex(16)
	switch action {
	case "edit":
		if !isAuthor {
			httpErr(w, errors.New("只有作者本人能修改"), http.StatusForbidden)
			return
		}
		var body struct{ Content store.Content }
		if err := readJSON(r, &body); err != nil {
			httpErr(w, err, http.StatusBadRequest)
			return
		}
		body.Content.Frame, body.Content.Seconds, body.Content.Shot = create.Content.Frame, create.Content.Seconds, create.Content.Shot
		if err := checkContent(st, &body.Content); err != nil {
			httpErr(w, err, http.StatusBadRequest)
			return
		}
		rec.Content = &body.Content
	case "delete":
		if !isAuthor {
			httpErr(w, errors.New("只有作者本人能删除"), http.StatusForbidden)
			return
		}
		rec.Video = create.Video
	default:
		http.NotFound(w, r)
		return
	}
	a.commit(w, st, me, rec)
}

var imageTypes = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

func (a *App) apiUploadImage(w http.ResponseWriter, r *http.Request) {
	st := a.getStore()
	if st == nil {
		httpErr(w, errors.New("还没有设置共享文件夹"), http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 30<<20+1))
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	if len(data) > 30<<20 {
		httpErr(w, errors.New("图片不能超过 30MB"), http.StatusBadRequest)
		return
	}
	ext, ok := imageTypes[http.DetectContentType(data)]
	if !ok {
		httpErr(w, errors.New("只支持 PNG、JPG、GIF、WebP 图片"), http.StatusBadRequest)
		return
	}
	name, err := st.PutImage(data, ext)
	if err != nil {
		httpErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"name": name})
}
