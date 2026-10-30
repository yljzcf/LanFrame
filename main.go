package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"lanreview/internal/platform"
)

//go:embed web
var webFS embed.FS

const appName = "lan-video-review"

// version 是程序的版本号，打包时由 build.sh 用 -ldflags "-X main.version=..." 写进来。
var version = "dev"

func main() {
	// 端口不能放在 49152 以上：Mac 和 Windows 会从那一段随机挑端口给对外连接用，随时可能被占
	port := flag.Int("port", 28380, "HTTP 端口")
	dataDir := flag.String("data", "", "配置目录，默认在系统的用户配置目录下")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	noTray := flag.Bool("no-tray", false, "不显示托盘图标（开发测试用）")
	child := flag.Bool("child", false, "内部使用：不转到后台，直接运行")
	autostart := flag.Bool("autostart", false, "内部使用：开机启动时由系统带上")
	restart := flag.Bool("restart", false, "内部使用：自动更新后重启，等旧版本让出端口")
	flag.Parse()

	defaultData := *dataDir == ""
	if defaultData {
		base, err := os.UserConfigDir()
		if err != nil {
			log.Fatal(err)
		}
		*dataDir = filepath.Join(base, appName)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	setupLog(*dataDir)
	if !*child && platform.DetachFromApp() {
		return
	}

	url := fmt.Sprintf("http://localhost:%d/", *port)
	var ln net.Listener
	var err error
	for try := 0; ; try++ {
		if ln, err = net.Listen("tcp", fmt.Sprintf(":%d", *port)); err == nil {
			break
		}
		// 自动更新后重启：旧版本还没退出，等它让出端口。等不到就算了，旧版本还在运行，不弹框
		if *restart {
			if try == 40 {
				log.Printf("等了 20 秒旧版本还没让出端口，新版本不启动：%v", err)
				return
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		// 程序已经在运行时，再次双击只打开界面
		if alreadyRunning(*port) {
			if *noBrowser || *autostart {
				log.Printf("程序已经在运行")
			} else {
				log.Printf("程序已经在运行，直接打开界面：%s", url)
				openBrowser(url)
			}
			return
		}
		// 端口可能只是被别的连接临时占着，等几秒再试
		if try == 4 {
			fatal(fmt.Sprintf("端口 %d 被其他程序占用，局域网审片无法启动。\n请关闭占用这个端口的程序后再试，或者重启电脑。", *port), err)
		}
		time.Sleep(time.Second)
	}

	app, err := newApp(*dataDir, *port)
	if err != nil {
		fatal("局域网审片无法启动。", err)
	}
	// 开机启动项里记着程序路径，程序被挪过位置的话这里顺便更新。
	// 只在双击打开（用默认配置目录）时更新：用 -data 另开的开发测试实例不能把启动项改成指向自己；
	// 开机启动本身带着 -data，路径一定是对的，也不用更新
	if defaultData && platform.AutostartEnabled() {
		if err := platform.SetAutostart(true, app.autostartArgs()); err != nil {
			log.Printf("更新开机启动项失败：%v", err)
		}
	}
	app.start()
	platform.SetKeepAwake(!app.paused())
	go func() { log.Fatal(http.Serve(ln, app.routes())) }()

	log.Printf("已启动 %s：%s（开机启动：%v，自动更新后重启：%v，服务暂停：%v）", version, url, *autostart, *restart, app.paused())
	switch {
	case *autostart && app.config().SilentStart:
		log.Printf("开机后静默运行，不打开浏览器")
	case !*noBrowser:
		openBrowser(url)
	}
	if *noTray {
		select {}
	}
	app.runTray(url)
}

// fatal 写日志、弹出错误提示框后退出。程序没有窗口，不弹框的话用户只会觉得"双击没反应"。
func fatal(msg string, err error) {
	log.Printf("%s（%v）", msg, err)
	platform.ShowError("局域网审片无法启动", msg)
	os.Exit(1)
}

// setupLog 把日志写到配置目录的 log.txt。程序没有命令行窗口时，出问题靠它排查。
func setupLog(dir string) {
	path := filepath.Join(dir, "log.txt")
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		os.Rename(path, filepath.Join(dir, "log.old.txt"))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	// 文件放在前面：没有命令行窗口时写 stderr 会失败，但不影响写文件
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}

// alreadyRunning 判断端口上是不是已经有一个审片程序在运行（而不是别的程序占着这个端口）。
func alreadyRunning(port int) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/peer/info", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var info peerInfo
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&info) == nil && info.App == appName
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("无法自动打开浏览器，请手动访问 %s", url)
	}
}
