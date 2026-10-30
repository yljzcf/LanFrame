// Package platform 放和操作系统打交道的部分：开机启动、防睡眠、错误弹窗、Mac 上转到后台运行、托盘图标。
// 每个功能按系统分成 _darwin、_windows、_other 三份文件。
package platform

// appID 是写进 Windows 注册表"启动"项时用的名字。
const appID = "lan-video-review"
