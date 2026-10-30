package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// MaxSize 是安装包大小的上限，防止有人用一个没完没了的文件把硬盘塞满。
const MaxSize = 200 << 20

const repo = "yljzcf/LanFrame"

// releasesURL 是 GitHub Release 的地址。环境变量 LANFRAME_UPDATE_BASE 可以换成别的地址，只给开发测试用。
func releasesURL() string {
	if b := os.Getenv("LANFRAME_UPDATE_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://github.com/" + repo + "/releases"
}

// LatestURL 是最新版本的更新清单。用 Release 的下载地址而不是 GitHub API，没有访问次数限制。
func LatestURL() string { return releasesURL() + "/latest/download/update.json" }

// FileURL 是某个版本的安装包在 GitHub 上的地址。
func FileURL(version, name string) string {
	return releasesURL() + "/download/v" + version + "/" + name
}

// Get 读取一个不大的文件（比如 update.json），最多 limit 字节。
func Get(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Download 把 url 下载到 dst，大小和 SHA-256 都要和清单一致；对不上时不留下任何文件。
func Download(ctx context.Context, client *http.Client, url string, f File, dst string) error {
	if f.Size <= 0 || f.Size > MaxSize {
		return errors.New("安装包大小不对")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
	case n != f.Size:
		err = fmt.Errorf("%s 下载到的大小不对", f.Name)
	case hex.EncodeToString(h.Sum(nil)) != f.SHA256:
		err = fmt.Errorf("%s 校验不通过，可能被篡改或者下载出错", f.Name)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// VerifyFile 检查本地文件和清单是否一致。换上新版本前再查一遍，防止缓存的文件被动过。
func VerifyFile(path string, f File) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(file, MaxSize+1))
	if err != nil {
		return err
	}
	if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s 校验不通过，可能被改动过", f.Name)
	}
	return nil
}
