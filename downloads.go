package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lanreview/internal/platform"
	"lanreview/internal/store"
	"lanreview/internal/update"
)

// 从同事电脑上拿文件：同事开放的是共享文件夹（见 files.go），这边由程序在后台下载到自己共享文件夹的
// "备份/来源/"里，网页关了也继续；断网、对方离线、程序重启后都从断点接着下。
// 拿来的视频成为原主人视频的备份（见 backups.go），原主人不在时同事也能从这里看。

const (
	partExt      = ".lanpart"       // 没下完的文件
	maxParallel  = 2                // 同时下载的文件数
	stallTimeout = 30 * time.Second // 这么久没收到数据就断开重试
	maxFails     = 10               // 连续失败这么多次（约 6 分钟）就不再自动重试，等用户点"重试"
)

type dlFile struct {
	Path  string `json:"path"`  // 对方共享文件夹里的相对路径
	Local string `json:"local"` // 存到本机的完整路径
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Done  bool   `json:"done,omitempty"`
	Err   string `json:"err,omitempty"` // 这个文件下载失败的原因，比如对方已经删了

	got     atomic.Int64 // 已经下载的字节数
	active  bool
	cancel  context.CancelFunc
	fails   int
	retryAt time.Time
	lastErr string
}

type dlJob struct {
	ID       string    `json:"id"`
	Peer     string    `json:"peer"`
	PeerName string    `json:"peerName"`
	Name     string    `json:"name"`  // 存到本机的名字
	Local    string    `json:"local"` // 存到本机的文件或文件夹（"备份/来源/…"）
	Dir      bool      `json:"dir,omitempty"`
	Files    []*dlFile `json:"files"`
	Created  int64     `json:"created"`
	Canceled bool      `json:"canceled,omitempty"`
	// 列表上点"备份视频"建的任务：按指纹拿，原主人（Peer）不在或者已经删了时，从在线的、存着备份的同事那里拿
	FP  string `json:"fp,omitempty"`
	Err string `json:"err,omitempty"` // 整个任务停下的原因，比如团队口令不一致、本机磁盘满了

	speed     float64 // 字节/秒
	sampleAt  time.Time
	sampleGot int64
	doneAt    time.Time // 这次运行里下完的时间，网页用来提示一次
}

// dlView 是给网页看的一项下载。
type dlView struct {
	ID       string   `json:"id"`
	FP       string   `json:"fp,omitempty"` // "备份视频"的任务才有
	PeerName string   `json:"peerName"`
	Name     string   `json:"name"`
	Dir      bool     `json:"dir"`
	State    string   `json:"state"` // queued running waiting paused retrying done partial failed canceled
	Err      string   `json:"err,omitempty"`
	Files    int      `json:"files"`
	Done     int      `json:"done"`
	Failed   []string `json:"failed,omitempty"` // 下载失败的文件和原因，最多 5 条
	Size     int64    `json:"size"`
	Got      int64    `json:"got"`
	Speed    float64  `json:"speed"`
	Created  int64    `json:"created"`
	DoneAt   int64    `json:"doneAt,omitempty"`
}

type downloads struct {
	path   string // downloads.json
	mu     sync.Mutex
	jobs   []*dlJob
	active int
	kickCh chan struct{}
}

func loadDownloads(dataDir string) *downloads {
	d := &downloads{path: filepath.Join(dataDir, "downloads.json"), kickCh: make(chan struct{}, 1)}
	data, err := os.ReadFile(d.path)
	if err == nil {
		if err := json.Unmarshal(data, &d.jobs); err != nil {
			log.Printf("下载列表读不了，从空的开始：%v", err)
			d.jobs = nil
		}
	}
	// 上次没下完的，进度从已经下好的部分算起
	for _, j := range d.jobs {
		for _, f := range j.Files {
			if st, err := os.Stat(f.Local + partExt); err == nil && !f.Done {
				f.got.Store(st.Size())
			}
		}
	}
	return d
}

// saveLocked 把下载列表存下来，程序重启或自动更新后接着下。调用时要持有 d.mu。
func (d *downloads) saveLocked() {
	data, _ := json.MarshalIndent(d.jobs, "", "  ")
	if err := store.WriteFileAtomic(d.path, data); err != nil {
		log.Printf("保存下载列表失败：%v", err)
	}
}

func (d *downloads) kick() {
	select {
	case d.kickCh <- struct{}{}:
	default:
	}
}

func (d *downloads) find(id string) *dlJob {
	for _, j := range d.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// stateLocked 是一项下载现在的状态。online 是对方在不在线。
func (j *dlJob) stateLocked(online, paused bool) string {
	if j.Canceled {
		return "canceled"
	}
	var done, failed, active, retrying int
	for _, f := range j.Files {
		switch {
		case f.Done:
			done++
		case f.Err != "":
			failed++
		case f.active:
			active++
		case f.fails > 0:
			retrying++
		}
	}
	switch {
	case done+failed == len(j.Files) && failed == 0:
		return "done"
	case done+failed == len(j.Files) && done > 0:
		return "partial"
	case done+failed == len(j.Files) || j.Err != "":
		return "failed"
	case active > 0:
		return "running"
	case paused:
		return "paused"
	case !online:
		return "waiting"
	case retrying > 0:
		return "retrying"
	}
	return "queued"
}

func (j *dlJob) finished() bool {
	for _, f := range j.Files {
		if !f.Done && f.Err == "" {
			return j.Canceled || j.Err != ""
		}
	}
	return true
}

// backupRoot 是共享文件夹里的"备份"，从同事那里拿的东西都存在这里。共享文件夹打不开时返回原因。
func (a *App) backupRoot() (string, error) {
	if a.getStore() == nil {
		if msg := a.shareError(); msg != "" {
			return "", errors.New(msg)
		}
		return "", errors.New("还没有设置共享文件夹")
	}
	return filepath.Join(a.config().ShareDir, store.BackupDirName), nil
}

// backupPath 是从同事那里拿的文件存到本机的位置：共享文件夹/备份/来源/剩下的路径。
// 对方"分享"里的，来源是对方的昵称，去掉"分享"这一层；对方"备份/X/"里的（转手的备份），
// 来源沿用 X。每一段都清理成 Win 和 Mac 都能用的名字。rel 是对方共享文件夹里的相对路径。
func backupPath(root, peerName, rel string) string {
	parts := strings.Split(rel, "/")
	source := peerName
	switch {
	case parts[0] == store.BackupDirName && len(parts) == 1:
		return root // 选的是整个"备份"，里面已经按来源分好了
	case parts[0] == store.BackupDirName:
		source, parts = parts[1], parts[2:]
	case parts[0] == store.OutDirName:
		parts = parts[1:]
	}
	segs := []string{root, safeName(source)}
	for _, c := range parts {
		segs = append(segs, safeName(c))
	}
	return filepath.Join(segs...)
}

// localError 把本机写文件失败的原因说成用户能照着做的话。
func localError(dir string, err error) error {
	if errors.Is(err, fs.ErrPermission) && runtime.GOOS == "darwin" {
		return errors.New("macOS 没有允许「局域网审片」把文件存到 " + dir + "。" +
			"请打开 系统设置 → 隐私与安全性 → 文件与文件夹，找到「局域网审片」，打开共享文件夹所在的文件夹（比如\"桌面文件夹\"），再点\"重试\"。")
	}
	return fmt.Errorf("没法把文件存到 %s：%v", dir, err)
}

// sameFile 判断本机已有的文件是不是就是对方的这个文件（大小一样，修改时间差不到 2 秒：有的磁盘只记到 2 秒）。
func sameFile(local string, size, mtime int64) bool {
	st, err := os.Stat(local)
	if err != nil || !st.Mode().IsRegular() || st.Size() != size {
		return false
	}
	d := st.ModTime().UnixMilli() - mtime
	return d > -2000 && d < 2000
}

// onlinePeer 返回在线的同事。
func (a *App) onlinePeer(id string) (Peer, bool) {
	for _, p := range a.peers.Snapshot() {
		if p.ID == id && p.Online {
			return p, true
		}
	}
	return Peer{}, false
}

// listPeerFiles 向同事要共享文件夹里的文件列表；deep 时把选中的文件夹展开成文件清单。
func (a *App) listPeerFiles(ctx context.Context, peerID, rel string, deep bool) ([]fileEntry, error) {
	p, ok := a.onlinePeer(peerID)
	if !ok {
		return nil, errors.New("对方不在线")
	}
	u := "http://" + p.Addr + "/peer/files?path=" + url.QueryEscape(rel)
	if deep {
		u += "&deep=1"
	}
	req, err := a.peerRequest(ctx, u)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errors.New("连不上对方，请确认对方的程序开着")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, errors.New(forbiddenText(resp.Body))
	case resp.StatusCode != http.StatusOK && !p.Files && !update.Newer(p.Version, "0.3.99"):
		return nil, errors.New("对方的程序版本太旧，还不能拿文件。等对方自动更新到 0.4.0 以后再试")
	case resp.StatusCode != http.StatusOK:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, errors.New(strings.TrimSpace(string(msg)))
	}
	var list []fileEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("对方返回的文件列表不对：%v", err)
	}
	return list, nil
}

// Windows 不允许的文件名（不分大小写，带不带扩展名都不行）
var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := 1; i <= 9; i++ {
		reservedNames[fmt.Sprintf("COM%d", i)] = true
		reservedNames[fmt.Sprintf("LPT%d", i)] = true
	}
}

// safeName 把对方的文件名改成在 Win 和 Mac 上都能用的名字：Mac 上允许的 ? * : " 等字符在 Windows 上不行，
// 结尾的句点和空格 Windows 会吞掉，CON、NUL 这类名字 Windows 不让建。
func safeName(name string) string {
	name = strings.TrimRight(reBadFileChars.ReplaceAllString(name, "_"), ". ")
	if name == "" {
		return "_"
	}
	if reservedNames[strings.ToUpper(strings.SplitN(name, ".", 2)[0])] {
		name = "_" + name
	}
	return name
}

// uniqueName 在 dir 里给 name 找一个没被占用的名字：已经有同名的就叫"名字 (1).扩展名"。
// taken 是这次已经分配出去、还没写到硬盘上的路径。
func uniqueName(dir, name string, isDir bool, taken map[string]bool) string {
	ext := ""
	if !isDir {
		ext = filepath.Ext(name)
	}
	base := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		p := filepath.Join(dir, n)
		if taken[strings.ToLower(p)] {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if _, err := os.Lstat(p + partExt); err == nil {
			continue
		}
		return p
	}
}

// takenPaths 是还没下完的文件占着的本机路径（不分大小写，Win 和 Mac 的磁盘默认都不分）。调用时要持有 d.mu。
func (d *downloads) takenPathsLocked() map[string]bool {
	taken := map[string]bool{}
	for _, j := range d.jobs {
		if j.finished() {
			continue
		}
		for _, f := range j.Files {
			if !f.Done && f.Err == "" {
				taken[strings.ToLower(f.Local)] = true
			}
		}
	}
	return taken
}

// startDownloads 从同事那里下载选中的文件和文件夹，每一项是一个下载任务。
// 存到"备份/来源/"里，和已有的合并：已经有一样的文件（大小和修改时间都一样）就不再下，
// 同名但不一样的存成"名字 (1)"，不覆盖。再拿一次同一个文件夹，等于把备份补全。
func (a *App) startDownloads(peerID string, paths []string) error {
	p, ok := a.onlinePeer(peerID)
	if !ok {
		return errors.New("对方不在线")
	}
	if len(paths) == 0 {
		return errors.New("没有选中文件")
	}
	dir, err := a.backupRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return localError(dir, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	type item struct {
		rel   string
		files []fileEntry
		dir   bool
	}
	var items []item
	var total int64
	for _, rel := range paths {
		if rel = path.Clean(rel); rel == "." || rel == "/" {
			return errors.New("请选择具体的文件或文件夹")
		}
		list, err := a.listPeerFiles(ctx, peerID, rel, true)
		if err != nil {
			return fmt.Errorf("%s：%v", path.Base(rel), err)
		}
		isDir := !(len(list) == 1 && list[0].Path == rel)
		items = append(items, item{rel, list, isDir})
		for _, f := range list {
			if !sameFile(backupPath(dir, p.Name, f.Path), f.Size, f.MTime) {
				total += f.Size
			}
		}
	}
	if free, err := platform.FreeSpace(dir); err == nil && uint64(total) > free {
		return fmt.Errorf("%s 所在的磁盘剩余空间不够：要下载 %s，只剩 %s", dir, humanSize(total), humanSize(int64(free)))
	}

	a.dl.mu.Lock()
	defer a.dl.mu.Unlock()
	taken := a.dl.takenPathsLocked()
	for _, it := range items {
		j := &dlJob{
			ID: store.RandomHex(8), Peer: peerID, PeerName: p.Name, Dir: it.dir,
			Local: backupPath(dir, p.Name, it.rel), Created: time.Now().UnixMilli(), Files: []*dlFile{},
		}
		j.Name = filepath.Base(j.Local)
		for _, f := range it.files {
			local := backupPath(dir, p.Name, f.Path)
			if rel, err := filepath.Rel(dir, local); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue // 不会发生：每一段都清理过了。保险起见，跳出"备份"的一律不要
			}
			df := &dlFile{Path: f.Path, Local: local, Size: f.Size, MTime: f.MTime}
			switch {
			case sameFile(local, f.Size, f.MTime) && !taken[strings.ToLower(local)]:
				df.Done = true // 已经备份过了
				df.got.Store(f.Size)
			case taken[strings.ToLower(local)] || fileExists(local):
				// 同名但不一样（或者 a? 和 a_ 清理后撞了），另起名字，不覆盖
				local = uniqueName(filepath.Dir(local), filepath.Base(local), false, taken)
				df.Local = local
			}
			taken[strings.ToLower(local)] = true
			j.Files = append(j.Files, df)
		}
		if it.dir && len(j.Files) == 0 {
			os.MkdirAll(j.Local, 0o755) // 空文件夹也建出来，免得用户以为没下载
		}
		a.dl.jobs = append(a.dl.jobs, j)
		log.Printf("开始从 %s 下载 %s（%d 个文件，%s）到 %s", p.Name, it.rel, len(j.Files), humanSize(sizeOf(j)), j.Local)
	}
	a.dl.saveLocked()
	a.dl.kick()
	return nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func sizeOf(j *dlJob) int64 {
	var n int64
	for _, f := range j.Files {
		n += f.Size
	}
	return n
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// downloadLoop 不停地把排队的文件交给下载，同时最多 maxParallel 个。
func (a *App) downloadLoop() {
	for {
		a.scheduleDownloads()
		select {
		case <-a.dl.kickCh:
		case <-time.After(time.Second):
		}
	}
}

// downloadSource 是这项下载这次从哪里拿：一般就是对方；"备份视频"先找原主人（文件还在的话），
// 不然找在线的、存着这个视频（包括备份）的同事。没有在线的返回空。
func downloadSource(j *dlJob, peers []Peer) (id, addr string) {
	for _, owner := range []bool{true, false} {
		for _, p := range peers {
			if !p.Online || owner != (p.ID == j.Peer) {
				continue
			}
			if j.FP == "" {
				return p.ID, p.Addr
			}
			for _, v := range p.Videos {
				if v.FP == j.FP && !v.Missing {
					return p.ID, p.Addr
				}
			}
		}
		if j.FP == "" {
			break
		}
	}
	return "", ""
}

func (a *App) scheduleDownloads() {
	// 暂停服务时不走局域网，下载也停下
	if a.paused() {
		return
	}
	peers := a.peers.Snapshot()
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for _, j := range d.jobs {
		if j.Canceled || j.Err != "" {
			continue
		}
		src, addr := downloadSource(j, peers)
		if addr == "" {
			continue
		}
		for _, f := range j.Files {
			if d.active >= maxParallel {
				return
			}
			if f.Done || f.Err != "" || f.active || now.Before(f.retryAt) {
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			f.active, f.cancel = true, cancel
			d.active++
			go a.runDownload(ctx, j, f, src, addr)
		}
	}
}

// 下载一个文件失败的几种情况
type (
	errGone  struct{ msg string } // 这个文件拿不到了（对方删了），其他文件照常下
	errLocal struct{ err error }  // 本机的问题（没权限、磁盘满了），整个任务停下等用户处理
	errJob   struct{ msg string } // 对方的问题（团队口令不一致、关了文件下载），整个任务停下
)

var errStalled = errors.New("30 秒没有收到数据，断开重试")

// retryBase 是第一次重试前等的时间，之后每次翻倍，最长 1 分钟。测试里改短。
var retryBase = 5 * time.Second

// forbiddenText 是对方拒绝时的原因：团队口令不一致，或者对方关闭了文件下载。
func forbiddenText(body io.Reader) string {
	msg, _ := io.ReadAll(io.LimitReader(body, 512))
	if strings.Contains(string(msg), errForbidden.Error()) {
		return "团队口令和对方的不一致，改成一样的再点\"重试\""
	}
	if m := strings.TrimSpace(string(msg)); m != "" {
		return m
	}
	return "对方拒绝了"
}

func (e errGone) Error() string  { return e.msg }
func (e errLocal) Error() string { return e.err.Error() }
func (e errJob) Error() string   { return e.msg }

func (a *App) runDownload(ctx context.Context, j *dlJob, f *dlFile, src, addr string) {
	target := "http://" + addr + "/peer/file?path=" + url.QueryEscape(f.Path)
	if j.FP != "" {
		target = "http://" + addr + "/peer/video/" + j.FP
	}
	progressed, err := a.fetchFile(ctx, src, f, target)
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	f.active, f.cancel = false, nil
	d.active--
	var gone errGone
	var local errLocal
	var job errJob
	switch {
	case err == nil:
		f.Done, f.fails, f.lastErr = true, 0, ""
		if j.finished() && !j.Canceled {
			j.doneAt = time.Now()
			log.Printf("从 %s 下载完了 %s", j.PeerName, j.Local)
		}
	case j.Canceled:
		os.Remove(f.Local + partExt)
	case errors.As(err, &gone) && j.FP == "": // "备份视频"的话这台没有了还可以换一台，照常重试
		f.Err = gone.msg
	case errors.As(err, &local):
		j.Err = localError(filepath.Dir(f.Local), local.err).Error()
		log.Printf("下载 %s 停下了：%v", f.Local, local.err)
	case errors.As(err, &job):
		j.Err = job.msg
	default:
		if progressed {
			f.fails = 0 // 断之前下到了数据，说明网络是通的，只是不稳
		}
		f.fails++
		f.lastErr = err.Error()
		if f.fails >= maxFails {
			f.Err = "多次重试都失败了：" + f.lastErr
		} else {
			f.retryAt = time.Now().Add(min(retryBase<<(f.fails-1), time.Minute))
		}
		log.Printf("下载 %s 失败（第 %d 次）：%v", f.Path, f.fails, err)
	}
	d.saveLocked()
	d.kick()
}

// fetchFile 下载一个文件到 本机路径.lanpart，下完核对大小、改回原名，再设成原文件的修改时间。
// 已经有 .lanpart 时从断点接着下；对方的文件改过的话（If-Range 对不上）对方会返回整个文件，从头下。
// progressed 表示这一次有没有收到数据。
func (a *App) fetchFile(ctx context.Context, peerID string, f *dlFile, target string) (progressed bool, err error) {
	part := f.Local + partExt
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		return false, errLocal{err}
	}
	var off int64
	if st, err := os.Stat(part); err == nil && st.Size() <= f.Size {
		off = st.Size()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := a.peerRequest(ctx, target)
	if err != nil {
		return false, err
	}
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		req.Header.Set("If-Range", time.UnixMilli(f.MTime).UTC().Format(http.TimeFormat))
	}
	resp, err := a.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	size, mtime := f.Size, f.MTime
	switch resp.StatusCode {
	case http.StatusPartialContent:
		var start, end, total int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start != off || total != f.Size {
			os.Remove(part)
			return false, errors.New("续传的位置对不上，从头下载")
		}
	case http.StatusOK:
		off = 0 // 没要续传，或者对方的文件改过了，从头下
		if resp.ContentLength >= 0 {
			size = resp.ContentLength
		}
		if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil && t.Unix() != time.UnixMilli(mtime).Unix() {
			mtime = t.UnixMilli()
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// 文件没改过（If-Range 对上了），.lanpart 已经是完整的，只差改名
		if off != f.Size {
			os.Remove(part)
			return false, errors.New("续传的位置对不上，从头下载")
		}
		return false, a.finishFile(peerID, f, part, size, mtime)
	case http.StatusNotFound:
		return false, errGone{"对方已经没有这个文件了"}
	case http.StatusForbidden:
		return false, errJob{forbiddenText(resp.Body)}
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("对方返回 %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	flag := os.O_WRONLY | os.O_CREATE
	if off == 0 {
		flag |= os.O_TRUNC
	}
	out, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return false, errLocal{err}
	}
	if _, err := out.Seek(off, io.SeekStart); err != nil {
		out.Close()
		return false, errLocal{err}
	}
	// 30 秒没有新数据就断开，交给重试
	stall := time.AfterFunc(stallTimeout, func() { cancel(errStalled) })
	defer stall.Stop()
	got := off
	f.got.Store(got)
	buf := make([]byte, 1<<20)
	body := io.LimitReader(resp.Body, size-off+1)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return progressed, errLocal{werr}
			}
			got += int64(n)
			f.got.Store(got)
			progressed = true
			stall.Reset(stallTimeout)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			if errors.Is(context.Cause(ctx), errStalled) {
				return progressed, errStalled
			}
			return progressed, rerr
		}
	}
	if err := out.Close(); err != nil {
		return progressed, errLocal{err}
	}
	if got != size {
		return progressed, fmt.Errorf("收到 %d 字节，应该是 %d 字节，接着下", got, size)
	}
	return progressed, a.finishFile(peerID, f, part, size, mtime)
}

// finishFile 把下完的 .lanpart 改回原名。下载期间有人在本机放了同名文件的话，换一个名字，不覆盖。
// 改名之前先看是不是同事的视频，是的话记成备份。
func (a *App) finishFile(peerID string, f *dlFile, part string, size, mtime int64) error {
	a.dl.mu.Lock()
	if size != f.Size || mtime != f.MTime {
		f.Size, f.MTime = size, mtime
	}
	if _, err := os.Lstat(f.Local); err == nil {
		f.Local = uniqueName(filepath.Dir(f.Local), filepath.Base(f.Local), false, nil)
	}
	local := f.Local
	a.dl.mu.Unlock()
	a.noteBackup(peerID, local, part, size)
	if err := os.Rename(part, local); err != nil {
		return errLocal{err}
	}
	t := time.UnixMilli(mtime)
	os.Chtimes(local, t, t) // 剪辑软件按修改时间认素材，和对方的保持一致
	if err := platform.MarkDownloaded(local); err != nil {
		log.Printf("给 %s 加来源标记失败：%v", local, err)
	}
	f.got.Store(size)
	return nil
}

// snapshot 是给网页看的下载列表，最新的在前面。
func (a *App) downloadsView() []dlView {
	peers := a.peers.Snapshot()
	paused := a.paused()
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	out := []dlView{}
	for i := len(d.jobs) - 1; i >= 0; i-- {
		j := d.jobs[i]
		v := dlView{
			ID: j.ID, FP: j.FP, PeerName: j.PeerName, Name: j.Name, Dir: j.Dir, Files: len(j.Files), Created: j.Created,
			Err: j.Err,
		}
		_, addr := downloadSource(j, peers)
		v.State = j.stateLocked(addr != "", paused)
		for _, f := range j.Files {
			v.Size += f.Size
			switch {
			case f.Done:
				v.Done++
				v.Got += f.Size
			case f.Err != "":
				if len(v.Failed) < 5 {
					v.Failed = append(v.Failed, f.Path+"："+f.Err)
				}
				v.Got += f.Size // 失败的不再算剩余
			default:
				v.Got += f.got.Load()
				if v.Err == "" && f.lastErr != "" && !f.active {
					v.Err = "正在重试：" + f.lastErr
				}
			}
		}
		// 速度：和上一次看的时候比，平滑一下
		if dt := now.Sub(j.sampleAt).Seconds(); dt >= 1 {
			if v.State == "running" && !j.sampleAt.IsZero() {
				j.speed = 0.6*float64(max(v.Got-j.sampleGot, 0))/dt + 0.4*j.speed
			} else {
				j.speed = 0
			}
			j.sampleAt, j.sampleGot = now, v.Got
		}
		v.Speed = j.speed
		if !j.doneAt.IsZero() {
			v.DoneAt = j.doneAt.UnixMilli()
		}
		out = append(out, v)
	}
	return out
}

// cancelDownload 取消一项下载：删掉没下完的部分，已经下完的文件保留。
func (a *App) cancelDownload(id string) error {
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	j := d.find(id)
	if j == nil {
		return errors.New("没有这项下载")
	}
	j.Canceled = true
	for _, f := range j.Files {
		if f.active {
			f.cancel() // 正在下的，停下以后 runDownload 删掉 .lanpart
		} else if !f.Done {
			os.Remove(f.Local + partExt)
		}
	}
	d.saveLocked()
	log.Printf("取消了从 %s 下载 %s", j.PeerName, j.Name)
	return nil
}

// retryDownload 让失败的文件重新下载，整个任务停下的原因也清掉。
func (a *App) retryDownload(id string) error {
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	j := d.find(id)
	if j == nil {
		return errors.New("没有这项下载")
	}
	j.Err, j.Canceled = "", false
	for _, f := range j.Files {
		if !f.Done {
			f.Err, f.fails, f.retryAt, f.lastErr = "", 0, time.Time{}, ""
		}
	}
	j.doneAt = time.Time{}
	d.saveLocked()
	d.kick()
	return nil
}

// revealDownload 在访达或资源管理器里显示下载到的东西。只接受下载列表里记着的路径。
func (a *App) revealDownload(id string) error {
	d := a.dl
	d.mu.Lock()
	j := d.find(id)
	var target string
	if j != nil {
		target = j.Local
		if !j.Dir && len(j.Files) == 1 {
			target = j.Files[0].Local // 下载期间同名的话改过名
		}
	}
	d.mu.Unlock()
	if j == nil {
		return errors.New("没有这项下载")
	}
	if _, err := os.Stat(target); err != nil {
		target = filepath.Dir(target) // 还没下完，显示存放的文件夹
		if _, err := os.Stat(target); err != nil {
			return errors.New("文件已经不在了")
		}
	}
	return platform.Reveal(target)
}

// clearDownloads 把已经结束的（下完、失败、取消）从列表里拿掉，不动硬盘上的文件。
func (a *App) clearDownloads() {
	d := a.dl
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := d.jobs[:0]
	for _, j := range d.jobs {
		if !j.finished() || hasActive(j) {
			keep = append(keep, j)
		}
	}
	d.jobs = keep
	d.saveLocked()
}

func hasActive(j *dlJob) bool {
	for _, f := range j.Files {
		if f.active {
			return true
		}
	}
	return false
}

// trashPath 把文件或文件夹移到废纸篓（Windows 是回收站）。测试里换掉，免得往真的废纸篓里放东西。
var trashPath = platform.Trash

// deleteDownload 把一项下载拿到的东西移到废纸篓，再从列表里拿掉，省得用户去"备份"里自己找着删。
// 文件夹里只有这项下载的文件时整个文件夹一起移走；还有别的东西（再拿一次时另存的、用户自己放的）时只移走这项下载的文件。
// 没下完的部分直接删掉。只动"备份"里的东西；还在下的要先取消。
func (a *App) deleteDownload(id string) error {
	root, err := a.backupRoot()
	if err != nil {
		return err
	}
	d := a.dl
	d.mu.Lock()
	j := d.find(id)
	if j == nil {
		d.mu.Unlock()
		return errors.New("没有这项下载")
	}
	if !j.finished() || hasActive(j) {
		d.mu.Unlock()
		return errors.New("还在下载，先取消再删除")
	}
	isDir, local := j.Dir, j.Local
	var done, parts []string
	for _, f := range j.Files {
		if f.Done {
			done = append(done, f.Local)
		} else {
			parts = append(parts, f.Local+partExt)
		}
	}
	d.mu.Unlock()

	for _, p := range append([]string{local}, done...) {
		if rel, err := filepath.Rel(root, p); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%s 不在共享文件夹的\"备份\"里，不能删除", p)
		}
	}
	for _, p := range parts {
		os.Remove(p)
	}
	targets := done
	if isDir && onlyFiles(local, done) {
		targets = []string{local}
	}
	for _, p := range targets {
		if !fileExists(p) {
			continue // 已经自己删掉了
		}
		if err := trashPath(p); err != nil {
			return fmt.Errorf("%s 没能移到%s：%v", filepath.Base(p), platform.TrashName, err)
		}
	}
	if isDir {
		// 只移走了一部分时，空下来的子文件夹（还有这项下载的文件夹本身）删掉
		for _, p := range done {
			for dir := filepath.Dir(p); dir == local || strings.HasPrefix(dir, local+string(filepath.Separator)); dir = filepath.Dir(dir) {
				if os.Remove(dir) != nil {
					break
				}
			}
		}
	}

	d.mu.Lock()
	for i, x := range d.jobs {
		if x == j {
			d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
			break
		}
	}
	d.saveLocked()
	d.mu.Unlock()
	log.Printf("把下载的 %s 移到了%s", local, platform.TrashName)
	go a.rescan() // 删掉的视频马上从列表里消失，备份按钮也变回来
	return nil
}

// onlyFiles 判断文件夹里是不是只有这些文件（不算隐藏文件和没下完的部分）。
func onlyFiles(dir string, files []string) bool {
	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	only := true
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		switch {
		case err != nil:
			only = false
			return filepath.SkipAll
		case e.IsDir() || store.HiddenName(e.Name()) || strings.HasSuffix(p, partExt):
			return nil
		case !want[p]:
			only = false
			return filepath.SkipAll
		}
		return nil
	})
	return only
}

// ---- 本机网页用的接口 ----

// apiPeerFiles 浏览同事共享文件夹里的一层。网页不能直接访问同事的 /peer/，由程序代为去要。
func (a *App) apiPeerFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := a.listPeerFiles(r.Context(), q.Get("peer"), q.Get("path"), false)
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, list)
}

func (a *App) apiStartDownloads(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Peer  string
		Paths []string
	}
	if err := readJSON(r, &body); err != nil {
		httpErr(w, errors.New("请求不对"), http.StatusBadRequest)
		return
	}
	if err := a.startDownloads(body.Peer, body.Paths); err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiDownloadAction：取消、重试、在访达或资源管理器里显示、删除（移到废纸篓）。
func (a *App) apiDownloadAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "cancel":
		err = a.cancelDownload(id)
	case "retry":
		err = a.retryDownload(id)
	case "reveal":
		err = a.revealDownload(id)
	case "delete":
		err = a.deleteDownload(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) apiClearDownloads(w http.ResponseWriter, r *http.Request) {
	a.clearDownloads()
	writeJSON(w, map[string]bool{"ok": true})
}

// apiOpenDownloadDir 打开"备份"文件夹。
func (a *App) apiOpenDownloadDir(w http.ResponseWriter, r *http.Request) {
	dir, err := a.backupRoot()
	if err == nil {
		err = platform.OpenFolder(dir)
	}
	if err != nil {
		httpErr(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// apiOpenShare 打开"分享"文件夹，告诉用户要给同事看的东西放在哪。
func (a *App) apiOpenShare(w http.ResponseWriter, r *http.Request) {
	if a.getStore() == nil {
		httpErr(w, errors.New("共享文件夹打不开"), http.StatusBadRequest)
		return
	}
	if err := platform.OpenFolder(filepath.Join(a.config().ShareDir, store.OutDirName)); err != nil {
		httpErr(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
