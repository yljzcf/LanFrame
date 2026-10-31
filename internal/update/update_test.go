package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testKey 把内置公钥换成测试用的，返回对应的私钥。
func testKey(t *testing.T) ed25519.PrivateKey {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetPublicKey(pub))
	return priv
}

func fileOf(name string, data []byte) File {
	sum := sha256.Sum256(data)
	return File{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func signed1(t *testing.T, priv ed25519.PrivateKey, version string, files map[string]File) []byte {
	data, err := Sign(Manifest{Version: version, Files: files}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.2.1", true},
		{"0.10.0", "0.9.9", true},
		{"1.0.0", "0.99.99", true},
		{"0.2.1", "0.2.1", false},
		{"0.2.0", "0.2.1", false},
		{"0.3.0", "dev", false},
		{"dev", "0.3.0", false},
		{"0.3", "0.2.0", false},
		{"0.03.0", "0.2.0", false},
		{"v0.3.0", "0.2.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v，应该是 %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSignAndParse(t *testing.T) {
	priv := testKey(t)
	files := map[string]File{"windows": fileOf("lan-video-review-windows.exe", []byte("exe"))}
	data := signed1(t, priv, "0.3.0", files)
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.3.0" || m.Files["windows"] != files["windows"] {
		t.Fatalf("解析结果不对：%+v", m)
	}

	// 改动清单内容：把版本号改大
	var s signed
	json.Unmarshal(data, &s)
	s.Manifest = strings.Replace(s.Manifest, "0.3.0", "9.9.9", 1)
	tampered, _ := json.Marshal(s)
	if _, err := Parse(tampered); err == nil {
		t.Fatal("被改过的清单应该验证不通过")
	}

	// 别的私钥签的
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Parse(signed1(t, other, "0.3.0", files)); err == nil {
		t.Fatal("别的私钥签的清单应该验证不通过")
	}

	// 签名对、但文件名带路径
	bad := map[string]File{"windows": fileOf("../evil.exe", []byte("exe"))}
	if _, err := Parse(signed1(t, priv, "0.3.0", bad)); err == nil {
		t.Fatal("带路径的文件名应该被拒绝")
	}
	if _, err := Parse(signed1(t, priv, "dev", files)); err == nil {
		t.Fatal("不正式的版本号应该被拒绝")
	}
}

func TestDownload(t *testing.T) {
	body := []byte(strings.Repeat("新版本", 1000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	dir := t.TempDir()
	ctx := context.Background()

	dst := filepath.Join(dir, "ok")
	if err := Download(ctx, srv.Client(), srv.URL, fileOf("ok", body), dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(body) {
		t.Fatal("下载的内容不对")
	}
	if err := VerifyFile(dst, fileOf("ok", body)); err != nil {
		t.Fatal(err)
	}

	for name, f := range map[string]File{
		"hash":  fileOf("hash", append([]byte("x"), body[1:]...)),
		"short": fileOf("short", body[:100]),
		"long":  fileOf("long", append(body, 'x')),
	} {
		dst := filepath.Join(dir, name)
		if err := Download(ctx, srv.Client(), srv.URL, f, dst); err == nil {
			t.Errorf("%s：内容和清单对不上，应该下载失败", name)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Errorf("%s：下载失败时不应该留下文件", name)
		}
		if _, err := os.Stat(dst + ".part"); err == nil {
			t.Errorf("%s：下载失败时不应该留下临时文件", name)
		}
	}
}

func TestCache(t *testing.T) {
	priv := testKey(t)
	dir := t.TempDir()
	exe := []byte("windows 程序")
	put := func(c *Cache, version string) {
		data := signed1(t, priv, version, map[string]File{"windows": fileOf("lan-video-review-windows.exe", exe)})
		m, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put(data, m); err != nil {
			t.Fatal(err)
		}
	}

	c := OpenCache(dir, "0.3.0")
	if c.Version() != "" {
		t.Fatal("空缓存不应该有版本")
	}
	put(c, "0.3.1")
	put(c, "0.3.2")
	if c.Version() != "0.3.2" {
		t.Fatalf("缓存版本是 %s，应该是 0.3.2", c.Version())
	}
	if _, err := os.Stat(filepath.Join(dir, "0.3.1")); err == nil {
		t.Fatal("换成新版本后旧版本的文件夹应该删掉")
	}
	if _, _, ok := c.Path("windows"); ok || c.Complete() != "" {
		t.Fatal("还没下载的安装包不应该算下载好了")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(exe) }))
	defer srv.Close()
	if err := c.Download(context.Background(), srv.Client(), srv.URL, "windows"); err != nil {
		t.Fatal(err)
	}
	if path, _, ok := c.Path("windows"); !ok || c.Files()["lan-video-review-windows.exe"] != path {
		t.Fatal("下载好的安装包应该能找到")
	}
	if c.Complete() != "0.3.2" {
		t.Fatal("安装包都下载好后，缓存的版本应该算下载全了")
	}
	if err := c.Download(context.Background(), srv.Client(), srv.URL, "darwin"); err == nil {
		t.Fatal("清单里没有的系统应该下载失败")
	}

	// 重新打开：比正在运行的版本新的留下
	if OpenCache(dir, "0.3.1").Version() != "0.3.2" {
		t.Fatal("重新打开后应该还是 0.3.2")
	}
	// 已经更新到同一个版本：留着给同事下载
	if OpenCache(dir, "0.3.2").Version() != "0.3.2" {
		t.Fatal("和正在运行的版本一样的缓存应该留下")
	}
	// 手动装了更新的版本：缓存没用了
	if OpenCache(dir, "0.4.0").Version() != "" {
		t.Fatal("比正在运行的版本旧的缓存应该删掉")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("旧缓存应该删干净，还剩 %d 个", len(entries))
	}
}
