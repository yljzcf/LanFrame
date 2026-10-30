// appicon 生成打包要用的程序图标，由 build.sh 调用：
//
//	go run ./tools/appicon -iconset dist/icon/AppIcon.iconset -syso rsrc_windows_amd64.syso
//
// -iconset 是 Mac 版的图标文件夹，build.sh 再用 iconutil 转成 .icns；
// -syso 是 Windows 版的图标资源，放在 main 包的目录里，编译 Windows 版时 Go 会自动把它链接进 .exe。
// 图标本身在 internal/platform/icon.go 里画，和托盘图标、网页的 favicon 是同一套图形。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"lanreview/internal/platform"
)

func main() {
	iconset := flag.String("iconset", "", "Mac 版的图标文件夹（.iconset）")
	syso := flag.String("syso", "", "Windows 版的图标资源（.syso）")
	flag.Parse()
	if *iconset == "" && *syso == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *iconset != "" {
		if err := writeIconset(*iconset); err != nil {
			log.Fatal(err)
		}
	}
	if *syso != "" {
		if err := os.WriteFile(*syso, windowsIconSyso(), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

// writeIconset 写出 iconutil 要的 10 张图：每种尺寸一张普通的、一张 2 倍的。
func writeIconset(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, s := range []int{16, 32, 128, 256, 512} {
		files := map[string]int{
			fmt.Sprintf("icon_%dx%d.png", s, s):    s,
			fmt.Sprintf("icon_%dx%d@2x.png", s, s): s * 2,
		}
		for name, size := range files {
			if err := os.WriteFile(filepath.Join(dir, name), platform.AppIcon(size, true), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
