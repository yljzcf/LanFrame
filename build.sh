#!/bin/sh
# 编译到 dist/：
#   lan-video-review-windows.exe  没有命令行窗口的 Windows 版
#   局域网审片.app                 Mac 版，Apple 芯片和 Intel 芯片通用
#   lan-video-review-mac.zip      打包好的 Mac 版，给下载页用（保留可执行权限）
# 可选：VERSION=0.3.0 ./build.sh 指定版本号，写进程序和 Mac 版的 Info.plist；不给的话沿用 packaging/Info.plist 里的
set -e
cd "$(dirname "$0")"
VERSION="${VERSION:-$(plutil -extract CFBundleShortVersionString raw packaging/Info.plist)}"
LDFLAGS="-s -w -X main.version=$VERSION"
rm -rf dist
mkdir -p dist

# 程序图标（见 tools/appicon）：Mac 版的 iconset，和 Windows 版的图标资源。
# 图标资源放在根目录，编译 Windows 版时 Go 会自动把它链接进去，打包完就删掉
trap 'rm -f rsrc_windows_amd64.syso' EXIT
go run ./tools/appicon -iconset dist/AppIcon.iconset -syso rsrc_windows_amd64.syso

GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS -H=windowsgui" -o dist/lan-video-review-windows.exe .

APP="dist/局域网审片.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
# Mac 的托盘要用 cgo。分别指定 C 编译器的目标芯片；最低系统版本和 Info.plist 一致（macOS 13，Go 1.26 起的最低要求），
# 否则 C 编译器会默认用本机的系统版本，旧系统打不开
export CGO_ENABLED=1 CGO_CFLAGS="-mmacosx-version-min=13.0" CGO_LDFLAGS="-mmacosx-version-min=13.0"
GOOS=darwin GOARCH=arm64 CC="clang -arch arm64" go build -ldflags "$LDFLAGS" -o dist/mac-arm64 .
GOOS=darwin GOARCH=amd64 CC="clang -arch x86_64" go build -ldflags "$LDFLAGS" -o dist/mac-amd64 .
lipo -create -output "$APP/Contents/MacOS/lan-video-review" dist/mac-arm64 dist/mac-amd64
rm dist/mac-arm64 dist/mac-amd64
cp packaging/Info.plist "$APP/Contents/"
plutil -replace CFBundleShortVersionString -string "$VERSION" "$APP/Contents/Info.plist"
plutil -replace CFBundleVersion -string "$VERSION" "$APP/Contents/Info.plist"
iconutil -c icns dist/AppIcon.iconset -o "$APP/Contents/Resources/AppIcon.icns"
rm -r dist/AppIcon.iconset
codesign --force --deep -s - "$APP"
(cd dist && ditto -c -k --keepParent "局域网审片.app" lan-video-review-mac.zip)
ls -lh dist
