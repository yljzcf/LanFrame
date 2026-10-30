package main

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 检查接口的几道防线：本机接口拒绝其他网站发来的修改请求，/peer/ 拒绝浏览器，局域网访问不了本机接口。
func TestRequestGuards(t *testing.T) {
	a, err := newApp(t.TempDir(), 28380)
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	sameOrigin := map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:28380"}
	cases := []struct {
		name, method, path, remote string
		header                     map[string]string
		want                       int
	}{
		{"本机网页的修改请求", "POST", "/api/rescan", "127.0.0.1:50000", sameOrigin, 200},
		{"其他网站借浏览器发的修改请求", "POST", "/api/rescan", "127.0.0.1:50000",
			map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"其他网站，浏览器只带了 Origin", "POST", "/api/rescan", "127.0.0.1:50000",
			map[string]string{"Origin": "https://evil.example"}, 403},
		{"局域网访问本机接口", "POST", "/api/rescan", "192.168.1.20:50000", nil, 403},
		{"其他电脑上的程序", "GET", "/peer/info", "192.168.1.20:50000", nil, 200},
		{"浏览器访问 /peer/（比如 DNS 重绑定）", "GET", "/peer/info", "127.0.0.1:50000", sameOrigin, 403},
		{"浏览器访问 /peer/ 下的视频", "GET", "/peer/videos", "192.168.1.20:50000",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"favicon", "GET", "/favicon.png", "127.0.0.1:50000", nil, 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://localhost:28380"+c.path, nil)
		r.RemoteAddr = c.remote
		for k, v := range c.header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s：%s %s 返回 %d，应该是 %d（%s）", c.name, c.method, c.path, w.Code, c.want, w.Body.String())
		}
	}
}

func TestFavicon(t *testing.T) {
	w := httptest.NewRecorder()
	favicon(w, httptest.NewRequest(http.MethodGet, "/favicon.png", nil))
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 是 %q", ct)
	}
	if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
}
