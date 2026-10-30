#!/bin/sh
# 编译到 dist/：
#   lan-video-review-windows.exe  没有命令行窗口的 Windows 版
#   局域网审片.app                 Mac 版，Apple 芯片和 Intel 芯片通用
#   lan-video-review-mac.zip      打包好的 Mac 版，给下载页用（保留可执行权限）
# 可选：VERSION=0.3.0 ./build.sh 会把版本号写进 Mac 版；不给的话沿用 packaging/Info.plist 里的
set -e
cd "$(dirname "$0")"
rm -rf dist
mkdir -p dist
GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -H=windowsgui" -o dist/lan-video-review-windows.exe .

APP="dist/局域网审片.app"
mkdir -p "$APP/Contents/MacOS"
# Mac 的托盘要用 cgo。分别指定 C 编译器的目标芯片；最低系统版本和 Info.plist 一致（macOS 13，Go 1.26 起的最低要求），
# 否则 C 编译器会默认用本机的系统版本，旧系统打不开
export CGO_ENABLED=1 CGO_CFLAGS="-mmacosx-version-min=13.0" CGO_LDFLAGS="-mmacosx-version-min=13.0"
GOOS=darwin GOARCH=arm64 CC="clang -arch arm64" go build -ldflags "-s -w" -o dist/mac-arm64 .
GOOS=darwin GOARCH=amd64 CC="clang -arch x86_64" go build -ldflags "-s -w" -o dist/mac-amd64 .
lipo -create -output "$APP/Contents/MacOS/lan-video-review" dist/mac-arm64 dist/mac-amd64
rm dist/mac-arm64 dist/mac-amd64
cp packaging/Info.plist "$APP/Contents/"
if [ -n "$VERSION" ]; then
	plutil -replace CFBundleShortVersionString -string "$VERSION" "$APP/Contents/Info.plist"
fi
codesign --force --deep -s - "$APP"
(cd dist && ditto -c -k --keepParent "局域网审片.app" lan-video-review-mac.zip)
ls -lh dist
