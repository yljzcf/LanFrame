package main

import (
	"log"
	"runtime"

	"fyne.io/systray"
	"lanreview/internal/platform"
)

// runTray 显示托盘图标（Mac 在菜单栏，Windows 在右下角通知区域），会一直阻塞到退出。
// 必须在主线程调用。
func (a *App) runTray(url string) {
	systray.Run(func() {
		systray.SetTooltip("局域网审片")
		mStatus := systray.AddMenuItem("", "")
		mStatus.Disable()
		mOpen := systray.AddMenuItem("打开界面", "在浏览器里打开审片界面")
		mUpdate := systray.AddMenuItem("", "换上已经下载好的新版本，程序会重启几秒钟")
		mUpdate.Hide()
		systray.AddSeparator()
		mRun := systray.AddMenuItemCheckbox("运行服务", "取消后程序留在后台，但不再分享视频、不再同步批注", true)
		mAuto := systray.AddMenuItemCheckbox("开机启动", "登录电脑时自动打开程序", false)
		mSilent := systray.AddMenuItemCheckbox("开机后静默运行", "开机启动时不弹出浏览器", false)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "退出后同事就看不了你的视频了")

		check := func(m *systray.MenuItem, on bool) {
			if on {
				m.Check()
			} else {
				m.Uncheck()
			}
		}
		refresh := func() {
			c := a.config()
			name, status := c.Nickname, "运行中"
			if name == "" {
				name = "还没设置"
			}
			if c.Paused {
				status = "服务已暂停"
			}
			mStatus.SetTitle("局域网审片 · " + name + " · " + status)
			auto := platform.AutostartEnabled()
			check(mRun, !c.Paused)
			check(mAuto, auto)
			check(mSilent, c.SilentStart)
			if auto {
				mSilent.Enable()
			} else {
				mSilent.Disable()
			}
			if !platform.AutostartSupported() {
				mAuto.Disable()
			}
			if u := a.updateState(); u.Ready && u.Blocked == "" {
				mUpdate.SetTitle("更新到 " + u.Latest)
				mUpdate.Show()
			} else {
				mUpdate.Hide()
			}
			if runtime.GOOS == "darwin" {
				systray.SetTemplateIcon(platform.TrayTemplateIcon(c.Paused), platform.TrayTemplateIcon(c.Paused))
			} else {
				systray.SetIcon(platform.TrayWindowsIcon(c.Paused))
			}
		}
		refresh()
		a.setOnChange(refresh)

		go func() {
			for {
				var err error
				select {
				case <-mOpen.ClickedCh:
					openBrowser(url)
				case <-mUpdate.ClickedCh:
					go a.applyUpdateFromTray()
				case <-mRun.ClickedCh:
					err = a.setPaused(!a.config().Paused)
				case <-mAuto.ClickedCh:
					err = a.setAutostart(!platform.AutostartEnabled())
				case <-mSilent.ClickedCh:
					err = a.setSilentStart(!a.config().SilentStart)
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
				if err != nil {
					log.Printf("托盘操作失败：%v", err)
				}
			}
		}()
	}, func() {})
}
