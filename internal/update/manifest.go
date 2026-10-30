// Package update 是自动更新：验证更新清单的签名、下载安装包、替换正在运行的程序。
// 新版本可能来自 GitHub，也可能来自局域网里的同事，所以一律靠签名判断真假：
// 发布时 CI 用私钥给更新清单签名，程序里只放公钥（见 key.go）。
// 每个系统一份的部分（替换程序、启动新程序）按 _darwin、_windows、_other 分开。
package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// File 是清单里的一个安装包。
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest 是一个版本的更新清单。Files 的键是系统名（runtime.GOOS 的 windows、darwin）。
type Manifest struct {
	Version string          `json:"version"`
	Files   map[string]File `json:"files"`
}

// signed 是 update.json 的格式。签的是清单原文的字节，不用管 JSON 怎么排版、字段什么顺序。
type signed struct {
	Manifest string `json:"manifest"`
	Sig      string `json:"sig"`
}

var (
	reFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	reSHA256   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parse 用内置公钥验证 update.json 的签名，再解析清单。
func Parse(data []byte) (*Manifest, error) {
	pub, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("程序内置的更新公钥不对")
	}
	var s signed
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, errors.New("更新清单的格式不对")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Sig)
	if err != nil || !ed25519.Verify(pub, []byte(s.Manifest), sig) {
		return nil, errors.New("更新清单的签名不对，可能是伪造的")
	}
	var m Manifest
	if err := json.Unmarshal([]byte(s.Manifest), &m); err != nil {
		return nil, errors.New("更新清单的格式不对")
	}
	if !Valid(m.Version) {
		return nil, fmt.Errorf("更新清单里的版本号 %q 不对", m.Version)
	}
	// 文件名会拿来拼路径、拼网址，只允许简单的名字
	for goos, f := range m.Files {
		if !reFileName.MatchString(f.Name) || f.Size <= 0 || f.Size > MaxSize || !reSHA256.MatchString(f.SHA256) {
			return nil, fmt.Errorf("更新清单里 %s 的安装包信息不对", goos)
		}
	}
	return &m, nil
}

// Sign 用私钥给清单签名，生成 update.json 的内容。给 tools/signupdate 用。
func Sign(m Manifest, priv ed25519.PrivateKey) ([]byte, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	s := signed{string(body), base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))}
	return json.MarshalIndent(s, "", "  ")
}

// parseVersion 解析 0.3.0 这样的版本号。开发时的 dev 等解析不了的版本返回 false。
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Valid 判断是不是正式的版本号。版本号不正式（比如开发时的 dev）的程序不参与自动更新。
func Valid(v string) bool {
	_, ok := parseVersion(v)
	return ok
}

// Newer 判断版本 a 是否比 b 新。有一个不是正式版本号时返回 false。
func Newer(a, b string) bool {
	va, ok1 := parseVersion(a)
	vb, ok2 := parseVersion(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}
