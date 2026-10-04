package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 断线续传相关的测试。v1.0.0 的毛病：一块传到一半断了，电脑上已经有这块的前半截，
// 网页从这块开头重发会被拒绝，连接被断开，浏览器读不到回应，传输从此卡死。

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// 重发电脑上已经有的部分：跳过重复的字节，接着写，不报错
func TestResendOverlapIsAccepted(t *testing.T) {
	s, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)
	data := randBytes(300)
	id := "overlap000000001"

	upload(t, c, ts.URL, id, "a.bin", 300, 0, data[:120]) // 第一块只传到一半
	resp, m := upload(t, c, ts.URL, id, "a.bin", 300, 0, data[:200])
	if resp.StatusCode != 200 || m["received"].(float64) != 200 {
		t.Fatalf("从块开头重发应该被接受并接着写到 200：%d %v", resp.StatusCode, m)
	}
	resp, m = upload(t, c, ts.URL, id, "a.bin", 300, 150, data[150:]) // 又重叠了 50 字节，并且是最后一块
	if resp.StatusCode != 200 || m["done"] != true {
		t.Fatalf("最后一块：%d %v", resp.StatusCode, m)
	}
	got, _ := os.ReadFile(filepath.Join(s.config().Dir, "a.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("跳过重复部分后文件内容不对")
	}
}

// 问「收到多少了」
func TestUploadStatus(t *testing.T) {
	_, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)
	id := "status0000000001"
	if _, m := do(t, c, "GET", ts.URL+"/api/upload?id="+id, nil); m["received"].(float64) != 0 {
		t.Fatalf("还没开始传应该是 0：%v", m)
	}
	upload(t, c, ts.URL, id, "s.bin", 100, 0, make([]byte, 60))
	if _, m := do(t, c, "GET", ts.URL+"/api/upload?id="+id, nil); m["received"].(float64) != 60 {
		t.Fatalf("应该收到了 60：%v", m)
	}
	upload(t, c, ts.URL, id, "s.bin", 100, 60, make([]byte, 40))
	if _, m := do(t, c, "GET", ts.URL+"/api/upload?id="+id, nil); m["done"] != true {
		t.Fatalf("传完以后应该告诉网页完成了：%v", m)
	}
}

// 网页以为电脑上的比实际多：回 409，而且浏览器一定要能读到这个回应（请求体要先读完）
func TestGapReturns409Readably(t *testing.T) {
	_, ts := newTestServer(t)
	conn := rawUpload(t, ts, "gap0000000000001", 32<<20, 8<<20, 8<<20, true)
	status, body := readResponse(t, conn, 10*time.Second)
	if status != 409 || !strings.Contains(body, `"received":0`) {
		t.Fatalf("应该读到 409 和 received:0，得到 %d %s", status, body)
	}
}

// 磁盘快满了：提前拒绝，并且浏览器能读到写着原因的回应
func TestDiskFullIsReadable(t *testing.T) {
	_, ts := newTestServer(t)
	old := freeSpace
	freeSpace = func(string) (uint64, error) { return 50 << 20, nil }
	defer func() { freeSpace = old }()
	conn := rawUpload(t, ts, "diskfull00000001", 1<<30, 0, 8<<20, true)
	status, body := readResponse(t, conn, 10*time.Second)
	if status != 507 || !strings.Contains(body, "硬盘只剩") {
		t.Fatalf("应该读到 507 和原因，得到 %d %s", status, body)
	}
}

// 旧连接半死不活地挂着（手机锁屏）：新请求一到，旧的马上让位，不用等
func TestStalledRequestIsPreempted(t *testing.T) {
	s, ts := newTestServer(t)
	id := "stalled000000001"
	conn := rawUpload(t, ts, id, 64<<20, 0, 16<<20, false) // 只发了 1 MB 就不动了
	defer conn.Close()
	time.Sleep(300 * time.Millisecond)

	c := client(t)
	login(t, c, ts.URL)
	start := time.Now()
	_, m := do(t, c, "GET", ts.URL+"/api/upload?id="+id, nil)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("问进度被旧连接卡了 %v", d)
	}
	if m["received"].(float64) != 1<<20 {
		t.Fatalf("旧连接写进去的 1 MB 应该保留：%v", m)
	}
	_ = s
}

// 一直不发数据的连接，过了 uploadIdle 就被放弃
func TestIdleUploadTimesOut(t *testing.T) {
	_, ts := newTestServer(t)
	old := uploadIdle
	uploadIdle = 500 * time.Millisecond
	defer func() { uploadIdle = old }()
	conn := rawUpload(t, ts, "idle000000000001", 64<<20, 0, 16<<20, false)
	defer conn.Close()
	start := time.Now()
	_, _ = readResponse(t, conn, 5*time.Second)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("空闲连接 %v 后还没被放弃", d)
	}
}

// rawUpload 手写一个上传请求：声明 bodyLen 字节的请求体，full 时全部发完，否则只发 1 MB 就停住
func rawUpload(t *testing.T, ts *httptest.Server, id string, size, offset, bodyLen int64, full bool) net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /api/upload?id=%s&name=x.bin&size=%d&offset=%d HTTP/1.1\r\nHost: %s\r\nX-LT: 1\r\nCookie: %s=%s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n",
		id, size, offset, addr, cookieName, testKey, bodyLen)
	send := bodyLen
	if !full {
		send = 1 << 20
	}
	go func() { _, _ = io.CopyN(conn, zeroReader{}, send) }()
	return conn
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func readResponse(t *testing.T, conn net.Conn, timeout time.Duration) (int, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// peek 只看不动：报告正在传的请求读到了多少，不会把它掐掉；正式问进度才会顶替它
func TestPeekReportsLivenessWithoutPreempting(t *testing.T) {
	_, ts := newTestServer(t)
	c := client(t)
	login(t, c, ts.URL)
	id := "peek000000000001"
	conn := rawUpload(t, ts, id, 64<<20, 0, 16<<20, false) // 发了 1 MB 就不动了
	defer conn.Close()
	time.Sleep(300 * time.Millisecond)

	_, m := do(t, c, "GET", ts.URL+"/api/upload?id="+id+"&peek=1", nil)
	if m["active"] != true || m["rx"].(float64) != 1<<20 {
		t.Fatalf("peek 应该看到正在传、已读 1 MB：%v", m)
	}
	gen := m["gen"]
	_, m = do(t, c, "GET", ts.URL+"/api/upload?id="+id+"&peek=1", nil)
	if m["active"] != true || m["gen"] != gen {
		t.Fatalf("peek 不该顶替正在传的请求：%v", m)
	}
	do(t, c, "GET", ts.URL+"/api/upload?id="+id, nil) // 正式问进度：顶替掉卡住的旧请求
	_, m = do(t, c, "GET", ts.URL+"/api/upload?id="+id+"&peek=1", nil)
	if m["active"] != false {
		t.Fatalf("旧请求被顶替后应该没有正在传的请求了：%v", m)
	}
}
