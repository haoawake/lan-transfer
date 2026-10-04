// 文件传输助手：在电脑上开一个小网页服务，同一个局域网里的手机、平板、电脑
// 用浏览器打开就能互传文件、图片、视频和文字，不用装 App，也不走外网。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// 发版时由 -ldflags "-X main.version=x.y.z" 写入
var version = "dev"

const (
	appID       = "lan-transfer"
	appName     = "文件传输助手"
	repoURL     = "https://github.com/haoawake/lan-transfer"
	defaultPort = 8686
)

func main() {
	port := flag.Int("port", 0, "监听的端口（默认 8686）")
	dir := flag.String("dir", "", "收到的文件存放在哪个文件夹")
	data := flag.String("data", "", "配置和聊天记录存放在哪个文件夹")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	initConsole()
	go logLoop()

	dataDir := *data
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	cfg, err := loadConfig(dataDir)
	if err != nil {
		fatal("读取配置失败：%v", err)
	}
	if *port > 0 {
		cfg.Port = *port
	}
	if *dir != "" {
		if abs, err := filepath.Abs(*dir); err == nil {
			cfg.Dir = abs
		}
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		fatal("没法创建接收文件夹 %s：%v", cfg.Dir, err)
	}

	ln, actual, err := listen(cfg.Port)
	if err != nil {
		var already alreadyRunning
		if errors.As(err, &already) {
			// 已经开着一个了：把那个的网页打开就行，不再起第二个
			url := fmt.Sprintf("http://localhost:%d/", int(already))
			fmt.Printf("\n  %s已经在运行了，正在打开它的网页：%s\n", appName, url)
			openURL(url)
			time.Sleep(2 * time.Second)
			return
		}
		fatal("没法监听端口 %d：%v", cfg.Port, err)
	}

	srv, err := newServer(cfg, dataDir, actual)
	if err != nil {
		fatal("%v", err)
	}
	hs := &http.Server{
		Handler:           srv.handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// 不设 ReadTimeout / WriteTimeout：几个 GB 的视频要传好几分钟，SSE 连接也是常开的
	}
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("网页服务出错：%v", err)
		}
	}()

	openPage := !*noBrowser && !cfg.NoBrowser
	printBanner(srv, actual, cfg.Port, openPage)
	if openPage {
		openURL(fmt.Sprintf("http://localhost:%d/", actual))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	srv.close()
}

// alreadyRunning 表示这个端口上跑着的就是本程序
type alreadyRunning int

func (a alreadyRunning) Error() string { return fmt.Sprintf("已经在端口 %d 上运行", int(a)) }

// listen 先试配置里的端口，被别的程序占了就往后找一个空的。
// 端口上跑着的如果是本程序自己，返回 alreadyRunning。
func listen(port int) (net.Listener, int, error) {
	var firstErr error
	for p := port; p < port+30 && p < 65536; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			return ln, p, nil
		}
		if isOurs(p) {
			return nil, 0, alreadyRunning(p)
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, 0, firstErr
}

func isOurs(port int) bool {
	c := http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var p struct {
		App string `json:"app"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&p)
	return p.App == appID
}

func printBanner(s *Server, port, wanted int, opened bool) {
	cfg := s.config()
	fmt.Printf("\n  %s %s\n", appName, version)
	fmt.Println("  " + strings.Repeat("─", 46))
	fmt.Println("  让手机和电脑连同一个 Wi-Fi，用手机扫电脑网页上的二维码；")
	fmt.Println("  或者在手机浏览器里输入下面的地址：")
	addrs := lanAddrs()
	if len(addrs) == 0 {
		fmt.Println("    （没找到局域网地址，请检查电脑有没有连上 Wi-Fi 或网线）")
	}
	for i, a := range addrs {
		if i == 3 {
			break
		}
		fmt.Printf("    http://%s:%d    （%s）\n", a.IP, port, a.Iface)
	}
	if !cfg.NoAuth {
		fmt.Printf("  访问码：%s（扫码进入不需要输入）\n", cfg.Code)
	}
	if port != wanted {
		fmt.Printf("  注意：端口 %d 被别的程序占用了，这次用的是 %d。\n", wanted, port)
	}
	fmt.Printf("\n  收到的文件保存在：%s\n", cfg.Dir)
	if free, err := diskFree(cfg.Dir); err == nil && free < 2<<30 {
		fmt.Printf("  注意：这个盘只剩 %s 了，大文件会传不进来。可以在网页右上角「···」里换到别的盘。\n", humanSize(int64(free)))
	}
	if runtime.GOOS == "windows" {
		fmt.Println("  第一次运行如果弹出 Windows 防火墙提示，请点「允许访问」，手机才能连上。")
	}
	if opened {
		fmt.Println("  电脑上的网页已经自动打开。关掉这个窗口，程序就会退出。")
	} else {
		fmt.Printf("  在电脑浏览器里打开 http://localhost:%d ，关掉这个窗口，程序就会退出。\n", port)
	}
	fmt.Println("  " + strings.Repeat("─", 46))
}

// ---------------------------------------------------------------- 日志

// 控制台在 Windows 上可能被「选中文字」卡住，日志走带缓冲的通道，
// 写不动就丢，绝不能让传输等着打印
var logCh = make(chan string, 256)

func logf(format string, a ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, a...)
	select {
	case logCh <- line:
	default:
	}
}

func logLoop() {
	for line := range logCh {
		fmt.Println("  " + line)
	}
}

// fatal 打印错误后退出。Windows 上是双击打开的控制台窗口，直接退出的话
// 窗口一闪就没了，用户根本看不到原因，所以先等一下回车。
func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\n  出错了："+format+"\n", a...)
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "\n  按回车键退出…")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}

func defaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "LanTransfer")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "LanTransfer-data")
	}
	return "LanTransfer-data"
}
