// signupdate 给发布的安装包生成签好名的更新清单 update.json，程序靠它确认新版本是真的。
//
// 生成一对新密钥（私钥写进文件，公钥打印出来，填进 internal/update/key.go）：
//
//	go run ./tools/signupdate -genkey 私钥文件
//
// 发布时签名（私钥从环境变量 UPDATE_SIGNING_KEY 读，安装包按扩展名对应系统：.exe 是 Windows，.zip 是 Mac）：
//
//	go run ./tools/signupdate -version 0.3.0 -o dist/update.json dist/lan-video-review-windows.exe dist/lan-video-review-mac.zip
//
// 签完会用程序内置的公钥验一遍，私钥和公钥对不上时报错，免得发布出去的版本谁都更新不了。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"lanreview/internal/update"
)

func main() {
	genkey := flag.String("genkey", "", "生成新密钥，私钥写进这个文件")
	version := flag.String("version", "", "版本号，比如 0.3.0")
	out := flag.String("o", "update.json", "输出的更新清单")
	flag.Parse()
	log.SetFlags(0)

	if *genkey != "" {
		if err := genKey(*genkey); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := sign(*version, *out, flag.Args()); err != nil {
		log.Fatal(err)
	}
}

func genKey(path string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err // 不覆盖已有的私钥
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	return nil
}

func sign(version, out string, files []string) error {
	if !update.Valid(version) {
		return fmt.Errorf("版本号 %q 不对，应该像 0.3.0 这样", version)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("环境变量 UPDATE_SIGNING_KEY 里没有有效的私钥")
	}
	m := update.Manifest{Version: version, Files: map[string]update.File{}}
	for _, path := range files {
		goos := map[string]string{".exe": "windows", ".zip": "darwin"}[filepath.Ext(path)]
		if goos == "" {
			return fmt.Errorf("不知道 %s 是哪个系统的安装包", path)
		}
		f, err := hashFile(path)
		if err != nil {
			return err
		}
		m.Files[goos] = f
	}
	if len(m.Files) == 0 {
		return errors.New("没有给安装包")
	}
	data, err := update.Sign(m, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		return err
	}
	if _, err := update.Parse(data); err != nil {
		return fmt.Errorf("签好的清单用内置公钥验证不通过（私钥和 internal/update/key.go 里的公钥对不上？）：%v", err)
	}
	return os.WriteFile(out, data, 0o644)
}

func hashFile(path string) (update.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return update.File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return update.File{}, err
	}
	return update.File{Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
