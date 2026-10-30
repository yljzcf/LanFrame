package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lanreview/internal/media"
	"lanreview/internal/store"
	"lanreview/internal/update"
)

const (
	mcastAddr     = "239.255.53.81:28381"
	onlineTimeout = 12 * time.Second
	// 校验不通过的记录（比如更新的版本才有的记录类型）隔这么久才再从同一台电脑下载一次，
	// 免得每轮同步都下载、拒绝、写一行日志
	rejectRetry = 10 * time.Minute
)

var errForbidden = errors.New("团队口令不一致")

type Peer struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Code        string              `json:"code"`
	Addr        string              `json:"addr"`
	Online      bool                `json:"online"`
	LastSeen    int64               `json:"lastSeen"`
	KeyMismatch bool                `json:"keyMismatch"`
	Version     string              `json:"version,omitempty"` // 对方运行的版本
	Update      string              `json:"update,omitempty"`  // 对方缓存里可以给别人下载的版本
	Videos      []*media.VideoEntry `json:"videos"`
}

type Peers struct {
	mu       sync.Mutex
	byID     map[string]*Peer
	syncing  map[string]bool
	rejected map[string]time.Time // "电脑 ID/记录 ID" -> 拒绝的时间。按电脑分开记，一台电脑上的副本坏了，还能从别的电脑拿到好的
}

func newPeers() *Peers {
	return &Peers{byID: map[string]*Peer{}, syncing: map[string]bool{}, rejected: map[string]time.Time{}}
}

func (ps *Peers) seen(id, name, addr string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.byID[id]
	if p == nil {
		p = &Peer{ID: id, Code: store.DeviceCode(id)}
		ps.byID[id] = p
		log.Printf("发现 %s（%s）", name, addr)
	}
	p.Name, p.Addr, p.LastSeen = name, addr, time.Now().UnixMilli()
}

func (ps *Peers) update(id string, fn func(p *Peer)) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := ps.byID[id]; p != nil {
		fn(p)
	}
}

// Snapshot 返回所有见过的电脑，按在线、昵称排序。
func (ps *Peers) Snapshot() []Peer {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]Peer, 0, len(ps.byID))
	for _, p := range ps.byID {
		c := *p
		c.Online = time.Since(time.UnixMilli(p.LastSeen)) < onlineTimeout
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (ps *Peers) anyOnline() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.byID {
		if time.Since(time.UnixMilli(p.LastSeen)) < onlineTimeout {
			return true
		}
	}
	return false
}

func (ps *Peers) reject(peer, rec string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.rejected[peer+"/"+rec] = time.Now()
}

func (ps *Peers) rejectedRecently(peer, rec string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	t, ok := ps.rejected[peer+"/"+rec]
	return ok && time.Since(t) < rejectRetry
}

func (ps *Peers) knownAddr(addr string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, p := range ps.byID {
		if p.Addr == addr && time.Since(time.UnixMilli(p.LastSeen)) < onlineTimeout {
			return true
		}
	}
	return false
}

// lanInterfaces 是能在局域网里组播的网卡。排除回环、点对点网卡（VPN 和代理软件的虚拟网卡）
// 和没有内网地址的网卡，否则开着代理时广播可能只发进了虚拟网卡。
func lanInterfaces() []net.Interface {
	var out []net.Interface
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 ||
			ifi.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 || len(lanIPv4(ifi)) == 0 {
			continue
		}
		out = append(out, ifi)
	}
	return out
}

func lanIPv4(ifi net.Interface) []net.IP {
	var out []net.IP
	addrs, _ := ifi.Addrs()
	for _, ad := range addrs {
		if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
			out = append(out, ipn.IP.To4())
		}
	}
	return out
}

type announce struct {
	App  string `json:"app"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

// discoveryLoop 每 5 秒在每张局域网网卡上组播一次"我在这里"，同时收听别人的广播。
func (a *App) discoveryLoop() {
	gaddr, err := net.ResolveUDPAddr("udp4", mcastAddr)
	if err != nil {
		log.Fatal(err)
	}
	listen := func(conn *net.UDPConn) {
		buf := make([]byte, 2048)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var m announce
			if a.paused() || json.Unmarshal(buf[:n], &m) != nil || m.App != appName || m.ID == a.me().ID || m.Port <= 0 {
				continue
			}
			a.peers.seen(m.ID, m.Name, net.JoinHostPort(src.IP.String(), strconv.Itoa(m.Port)))
		}
	}
	// 默认网卡上的监听，同一台电脑上的多个实例也靠它互相看到
	if conn, err := net.ListenMulticastUDP("udp4", nil, gaddr); err == nil {
		go listen(conn)
	} else {
		log.Printf("组播监听失败，自动发现不可用，可以手动添加 IP：%v", err)
	}
	conns := map[int]*net.UDPConn{} // 网卡序号 -> 绑定在这张网卡上的组播连接
	for i := 0; ; i++ {
		if i%6 == 0 { // 每 30 秒重新看一次网卡，换 WiFi、插网线后能自动用上
			for _, ifi := range lanInterfaces() {
				if conns[ifi.Index] != nil {
					continue
				}
				conn, err := net.ListenMulticastUDP("udp4", &ifi, gaddr)
				if err != nil {
					continue
				}
				log.Printf("在网卡 %s 上收发组播", ifi.Name)
				conns[ifi.Index] = conn
				go listen(conn)
			}
		}
		if c := a.config(); c.Nickname != "" && !c.Paused {
			msg, _ := json.Marshal(announce{appName, a.me().ID, c.Nickname, a.port})
			if conn, err := net.DialUDP("udp4", nil, gaddr); err == nil {
				conn.Write(msg)
				conn.Close()
			}
			for idx, conn := range conns {
				if _, err := conn.WriteToUDP(msg, gaddr); err != nil {
					conn.Close()
					delete(conns, idx)
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

type peerInfo struct {
	App     string `json:"app"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Locked  bool   `json:"locked"`            // 对方设了团队口令
	Version string `json:"version,omitempty"` // 0.3.0 起才有
	Update  string `json:"update,omitempty"`  // 缓存里可以给别人下载的版本，见 update.go
}

func (a *App) fetchInfo(ctx context.Context, addr string) (*peerInfo, error) {
	var info peerInfo
	if err := a.getJSON(ctx, "http://"+addr+"/peer/info", &info); err != nil {
		return nil, err
	}
	if info.App != appName || info.ID == "" {
		return nil, fmt.Errorf("%s 不是审片程序", addr)
	}
	return &info, nil
}

// addManual 手动添加一台电脑，地址可以省略端口。
func (a *App) addManual(addr string) (*peerInfo, error) {
	addr = strings.TrimSpace(addr)
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, strconv.Itoa(a.port))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	info, err := a.fetchInfo(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：请确认对方程序已打开、在同一个局域网，并且防火墙已允许", addr)
	}
	if info.ID == a.me().ID {
		return nil, fmt.Errorf("%s 是本机", addr)
	}
	a.peers.seen(info.ID, info.Name, addr)
	c := a.config()
	for _, m := range c.ManualPeers {
		if m == addr {
			return info, nil
		}
	}
	c.ManualPeers = append(c.ManualPeers, addr)
	return info, a.saveConfig(c)
}

func (a *App) manualLoop() {
	for {
		for _, addr := range a.config().ManualPeers {
			if a.paused() || a.peers.knownAddr(addr) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if info, err := a.fetchInfo(ctx, addr); err == nil && info.ID != a.me().ID {
				a.peers.seen(info.ID, info.Name, addr)
			}
			cancel()
		}
		time.Sleep(10 * time.Second)
	}
}

// subnetScanLoop 是组播被屏蔽时的兜底：一台在线的都没有时，挨个询问本网段的地址。
// 见过但已经离线的不算，对方换了 IP 也能重新找到。
func (a *App) subnetScanLoop() {
	time.Sleep(10 * time.Second)
	for {
		if c := a.config(); !a.peers.anyOnline() && c.Nickname != "" && !c.Paused {
			a.scanSubnets()
		}
		time.Sleep(2 * time.Minute)
	}
}

func (a *App) scanSubnets() {
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	for _, ip := range a.lanAddrs() {
		base := net.ParseIP(ip).To4()
		for i := 1; i < 255; i++ {
			target := net.IPv4(base[0], base[1], base[2], byte(i)).String()
			if target == ip {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(addr string) {
				defer func() { <-sem; wg.Done() }()
				ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
				defer cancel()
				if info, err := a.fetchInfo(ctx, addr); err == nil && info.ID != a.me().ID {
					a.peers.seen(info.ID, info.Name, addr)
				}
			}(net.JoinHostPort(target, strconv.Itoa(a.port)))
		}
	}
	wg.Wait()
}

func (a *App) syncLoop() {
	for {
		if a.paused() {
			time.Sleep(3 * time.Second)
			continue
		}
		for _, p := range a.peers.Snapshot() {
			a.peers.mu.Lock()
			busy := a.peers.syncing[p.ID]
			a.peers.syncing[p.ID] = true
			a.peers.mu.Unlock()
			if busy {
				continue
			}
			go func(p Peer) {
				a.syncPeer(p)
				a.peers.mu.Lock()
				delete(a.peers.syncing, p.ID)
				a.peers.mu.Unlock()
			}(p)
		}
		time.Sleep(3 * time.Second)
	}
}

// syncPeer 拉取对方的视频列表，再补齐自己缺少的批注记录和图片。
func (a *App) syncPeer(p Peer) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := a.fetchInfo(ctx, p.Addr)
	if err != nil || info.ID != p.ID || info.ID == a.me().ID {
		return
	}
	a.peers.seen(info.ID, info.Name, p.Addr)
	a.peers.update(p.ID, func(p *Peer) { p.Version, p.Update = info.Version, info.Update })
	if update.Newer(info.Update, a.upd.known()) {
		a.upd.kick(updateKick{}) // 对方有更新的版本，去要一份
	}
	var videos []*media.VideoEntry
	err = a.getJSON(ctx, "http://"+p.Addr+"/peer/videos", &videos)
	a.peers.update(p.ID, func(p *Peer) {
		p.KeyMismatch = errors.Is(err, errForbidden)
		if err == nil {
			p.Videos = videos
		} else if p.KeyMismatch {
			p.Videos = nil
		}
	})
	if err != nil {
		return
	}
	st := a.getStore()
	if st == nil {
		return
	}
	var stubs []store.RecordStub
	if a.getJSON(ctx, "http://"+p.Addr+"/peer/records", &stubs) != nil {
		return
	}
	// 先拿删除记录，被删的批注就不用再下载了
	sort.SliceStable(stubs, func(i, j int) bool { return stubs[i].Type == "delete" && stubs[j].Type != "delete" })
	for _, s := range stubs {
		if !store.ValidRecordID(s.ID) || st.Has(s.ID) || (s.Type != "delete" && st.IsDeleted(s.Ann)) || a.peers.rejectedRecently(p.ID, s.ID) {
			continue
		}
		var r store.Record
		if err := a.getJSON(ctx, "http://"+p.Addr+"/peer/record/"+s.ID, &r); err != nil {
			continue
		}
		if r.ID != s.ID {
			a.peers.reject(p.ID, s.ID)
			continue
		}
		if _, err := st.Add(&r); err != nil {
			a.peers.reject(p.ID, s.ID)
			log.Printf("拒绝来自 %s 的记录 %s：%v", p.Name, s.ID, err)
		}
	}
	for _, name := range st.MissingImages() {
		data, err := a.getBytes(ctx, "http://"+p.Addr+"/peer/image/"+name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != strings.SplitN(name, ".", 2)[0] {
			continue
		}
		store.WriteFileAtomic(st.ImagePath(name), data)
	}
}

// teamToken 是团队口令的哈希，放在请求头里，避免口令原文在局域网里传。
func teamToken(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(appName + "|" + key))
	return hex.EncodeToString(sum[:16])
}

func (a *App) peerRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if t := teamToken(a.config().TeamKey); t != "" {
		req.Header.Set("X-Team-Key", t)
	}
	return req, nil
}

func (a *App) getBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := a.peerRequest(ctx, url)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, errForbidden
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func (a *App) getJSON(ctx context.Context, url string, v any) error {
	data, err := a.getBytes(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
