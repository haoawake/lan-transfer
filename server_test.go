package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "0123456789abcdef0123456789abcdef"

// newTestServer 起一个「访客视角」的服务：本机不自动信任，必须登录
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	data, recv := t.TempDir(), t.TempDir()
	cfg := &Config{Port: defaultPort, Dir: recv, Key: testKey, Code: "123456", path: filepath.Join(data, "config.json")}
	s, err := newServer(cfg, data, defaultPort)
	if err != nil {
		t.Fatal(err)
	}
	s.noHostTrust = true
	ts := httptest.NewServer(s.handler())
	t.Cleanup(func() {
		ts.Close()
		s.close()
	})
	return s, ts
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func do(t *testing.T, c *http.Client, method, url string, body io.Reader) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("X-LT", "1")
	req.Header.Set("X-Device-Id", "testdevice01")
	req.Header.Set("X-Device-Name", "%E6%B5%8B%E8%AF%95") // 「测试」
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp, m
}

func login(t *testing.T, c *http.Client, base string) {
	t.Helper()
	resp, _ := do(t, c, "POST", base+"/api/login", strings.NewReader(`{"code":"123456"}`))
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
}

func TestAuth(t *testing.T) {
	_, ts := newTestServer(t)
	c := client(t)

	if resp, _ := do(t, c, "GET", ts.URL+"/api/sync", nil); resp.StatusCode != 401 {
		t.Fatalf("未登录应该 401，得到 %d", resp.StatusCode)
	}
	// 页面本身不需要登录
	if resp, _ := do(t, c, "GET", ts.URL+"/", nil); resp.StatusCode != 200 {
		t.Fatalf("首页 %d", resp.StatusCode)
	}
	if resp, _ := do(t, c, "POST", ts.URL+"/api/login", strings.NewReader(`{"code":"000000"}`)); resp.StatusCode != 403 {
		t.Fatalf("错误访问码应该 403，得到 %d", resp.StatusCode)
	}
	login(t, c, ts.URL)
	resp, m := do(t, c, "GET", ts.URL+"/api/sync", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("登录后 sync %d", resp.StatusCode)
	}
	if m["host"] != nil {
		t.Fatal("访客不该拿到 host 信息（里面有访问码）")
	}
	// 访客不能动电脑上的设置
	if resp, _ := do(t, c, "POST", ts.URL+"/api/host/settings", strings.NewReader(`{"noAuth":true}`)); resp.StatusCode != 403 {
		t.Fatalf("访客改设置应该 403，得到 %d", resp.StatusCode)
	}
	if resp, _ := do(t, c, "GET", ts.URL+"/api/qr", nil); resp.StatusCode != 403 {
		t.Fatalf("访客拿二维码应该 403，得到 %d", resp.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, ts := newTestServer(t)
	c := client(t)
	for i := 0; i < 5; i++ {
		do(t, c, "POST", ts.URL+"/api/login", strings.NewReader(`{"code":"999999"}`))
	}
	if resp, _ := do(t, c, "POST", ts.URL+"/api/login", strings.NewReader(`{"code":"123456"}`)); resp.StatusCode != 429 {
		t.Fatalf("连错 5 次后应该被锁，得到 %d", resp.StatusCode)
	}
}

func TestKeyLink(t *testing.T) {
	_, ts := newTestServer(t)
	c := client(t)
	resp, _ := do(t, c, "GET", ts.URL+"/?k="+testKey, nil)
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
		t.Fatalf("扫码链接应该跳回首页，得到 %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := do(t, c, "GET", ts.URL+"/api/sync", nil); resp.StatusCode != 200 {
		t.Fatalf("扫码后应该已登录，得到 %d", resp.StatusCode)
	}
	c2 := client(t)
	do(t, c2, "GET", ts.URL+"/?k=wrong", nil)
	if resp, _ := do(t, c2, "GET", ts.URL+"/api/sync", nil); resp.StatusCode != 401 {
		t.Fatal("错误的密钥不该登录")
	}
}

func TestCSRFHeader(t *testing.T) {
	_, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)
	req, _ := http.NewRequest("POST", ts.URL+"/api/text", strings.NewReader(`{"text":"hi"}`))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("没有 X-LT 头的写请求应该被拒，得到 %d", resp.StatusCode)
	}
}

func upload(t *testing.T, c *http.Client, base, id, name string, size, offset int, data []byte) (*http.Response, map[string]any) {
	return do(t, c, "POST", fmt.Sprintf("%s/api/upload?id=%s&name=%s&size=%d&offset=%d", base, id, name, size, offset), bytes.NewReader(data))
}

func TestChunkedUploadResumeAndDownload(t *testing.T) {
	s, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)

	data := bytes.Repeat([]byte("0123456789"), 25) // 250 字节
	id := "upload0000000001"

	resp, m := upload(t, c, ts.URL, id, "%E7%85%A7%E7%89%87.jpg", 250, 0, data[:100])
	if resp.StatusCode != 200 || m["received"].(float64) != 100 {
		t.Fatalf("第一块：%d %v", resp.StatusCode, m)
	}
	// 网页以为没发成功，从 0 重发：应该告诉它电脑上已经有 100 字节了
	resp, m = upload(t, c, ts.URL, id, "%E7%85%A7%E7%89%87.jpg", 250, 0, data[:100])
	if resp.StatusCode != 409 || m["received"].(float64) != 100 {
		t.Fatalf("重复的块应该 409：%d %v", resp.StatusCode, m)
	}
	upload(t, c, ts.URL, id, "%E7%85%A7%E7%89%87.jpg", 250, 100, data[100:200])
	resp, m = upload(t, c, ts.URL, id, "%E7%85%A7%E7%89%87.jpg", 250, 200, data[200:])
	if resp.StatusCode != 200 || m["done"] != true {
		t.Fatalf("最后一块：%d %v", resp.StatusCode, m)
	}
	item := m["item"].(map[string]any)
	if item["name"] != "照片.jpg" || item["from"] != "测试" || item["path"] != nil {
		t.Fatalf("记录不对：%v", item)
	}
	got, err := os.ReadFile(filepath.Join(s.config().Dir, "照片.jpg"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("文件内容不对：%v", err)
	}
	// 完成后再发同一个 id（响应丢了的情况）：直接告诉它完成了，不会多出一个文件
	resp, m = upload(t, c, ts.URL, id, "%E7%85%A7%E7%89%87.jpg", 250, 200, data[200:])
	if resp.StatusCode != 200 || m["done"] != true {
		t.Fatalf("重复完成：%d %v", resp.StatusCode, m)
	}

	// 断点续传 / 视频拖进度条用的 Range
	req, _ := http.NewRequest("GET", ts.URL+"/f/"+id+"/x.jpg", nil)
	req.Header.Set("Range", "bytes=10-19")
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 206 || string(part) != "0123456789" {
		t.Fatalf("Range 下载：%d %q", r2.StatusCode, part)
	}
	if cd := r2.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") || !strings.Contains(cd, "%E7%85%A7") {
		t.Fatalf("Content-Disposition 不对：%s", cd)
	}

	// 没有 cookie 的下载器：带签名可以，不带不行
	anon := &http.Client{}
	sig := item["sig"].(string)
	r3, _ := anon.Get(ts.URL + "/f/" + id + "/x.jpg?dl=1&s=" + sig)
	r3.Body.Close()
	if r3.StatusCode != 200 || !strings.HasPrefix(r3.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("签名下载：%d %s", r3.StatusCode, r3.Header.Get("Content-Disposition"))
	}
	r4, _ := anon.Get(ts.URL + "/f/" + id + "/x.jpg")
	r4.Body.Close()
	if r4.StatusCode != 401 {
		t.Fatalf("没签名应该 401，得到 %d", r4.StatusCode)
	}

	// 同名文件不覆盖
	resp, m = upload(t, c, ts.URL, "upload0000000002", "%E7%85%A7%E7%89%87.jpg", 3, 0, []byte("abc"))
	if m["done"] != true {
		t.Fatalf("第二个同名文件：%d %v", resp.StatusCode, m)
	}
	if _, err := os.Stat(filepath.Join(s.config().Dir, "照片 (1).jpg")); err != nil {
		t.Fatal("同名文件应该存成「照片 (1).jpg」")
	}

	// 删除记录时连文件一起删
	resp, _ = do(t, c, "POST", ts.URL+"/api/delete", strings.NewReader(`{"ids":["`+id+`"]}`))
	if resp.StatusCode != 200 {
		t.Fatalf("删除 %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(s.config().Dir, "照片.jpg")); !os.IsNotExist(err) {
		t.Fatal("删除记录后文件应该也没了")
	}
}

func TestEmptyFile(t *testing.T) {
	s, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)
	_, m := upload(t, c, ts.URL, "emptyfile0000001", "empty.txt", 0, 0, nil)
	if m["done"] != true {
		t.Fatalf("空文件：%v", m)
	}
	if st, err := os.Stat(filepath.Join(s.config().Dir, "empty.txt")); err != nil || st.Size() != 0 {
		t.Fatal("空文件没存下来")
	}
}

func TestTextIdempotent(t *testing.T) {
	s, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)
	body := `{"id":"textmsg000000001","text":"你好 https://example.com"}`
	do(t, c, "POST", ts.URL+"/api/text", strings.NewReader(body))
	resp, m := do(t, c, "POST", ts.URL+"/api/text", strings.NewReader(body))
	if resp.StatusCode != 200 || m["text"] != "你好 https://example.com" {
		t.Fatalf("文字：%d %v", resp.StatusCode, m)
	}
	if items, _ := s.store.Page(0, 10); len(items) != 1 {
		t.Fatalf("重发同一条消息应该只有一条记录，现在有 %d 条", len(items))
	}
	if resp, _ := do(t, c, "POST", ts.URL+"/api/text", strings.NewReader(`{"text":"   "}`)); resp.StatusCode != 400 {
		t.Fatal("空白内容应该被拒")
	}
}

func TestHostDetection(t *testing.T) {
	s, _ := newTestServer(t)
	s.noHostTrust = false
	cases := []struct {
		remote, host string
		want         bool
	}{
		{"127.0.0.1:5000", "localhost:8686", true},
		{"127.0.0.1:5000", "127.0.0.1:8686", true},
		{"[::1]:5000", "[::1]:8686", true},
		{"127.0.0.1:5000", "evil.example.com:8686", false}, // DNS 重绑定
		{"192.0.2.55:5000", "192.0.2.1:8686", false},       // 别的设备
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if got := s.isHost(r); got != c.want {
			t.Errorf("remote=%s host=%s：得到 %v，应该是 %v", c.remote, c.host, got, c.want)
		}
	}
}

func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "history.json")
	real := filepath.Join(dir, "a.txt")
	os.WriteFile(real, []byte("x"), 0o644)
	s, _ := openStore(file)
	s.Add(&Item{ID: "aaaaaaaa1", Type: "text", Text: "hi"})
	s.Add(&Item{ID: "aaaaaaaa2", Type: "file", Name: "a.txt", Path: real})
	s.Add(&Item{ID: "aaaaaaaa3", Type: "file", Name: "gone.txt", Path: filepath.Join(dir, "gone.txt")})
	s.Close()

	s2, err := openStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	items, more := s2.Page(0, 10)
	if len(items) != 2 || more {
		t.Fatalf("重新打开后应该剩 2 条（文件被删的那条去掉），得到 %d", len(items))
	}
	if items[0].Time >= items[1].Time {
		t.Fatal("时间应该严格递增")
	}
}

func TestListenDetectsRunningInstance(t *testing.T) {
	s, _ := newTestServer(t)
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go http.Serve(ln, s.handler())
	defer ln.Close()

	_, _, err = listen(port)
	var already alreadyRunning
	if !errors.As(err, &already) || int(already) != port {
		t.Fatalf("端口上已经跑着本程序时应该返回 alreadyRunning，得到 %v", err)
	}
}
