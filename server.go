package main

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web
var webFS embed.FS

const cookieName = "lt_key"

type Server struct {
	mu      sync.RWMutex // 保护 cfg
	cfg     *Config
	dataDir string
	port    int

	store   *Store
	hub     *Hub
	assets  map[string]*asset
	limiter *limiter
	uploads keyedMutex
	finalMu sync.Mutex
	local   localIPs

	// noHostTrust 让本机也当成普通设备，只在测试里用
	noHostTrust bool
}

func newServer(cfg *Config, dataDir string, port int) (*Server, error) {
	store, err := openStore(filepath.Join(dataDir, "history.json"))
	if err != nil {
		return nil, fmt.Errorf("读取聊天记录失败：%v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "thumbs"), 0o755); err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		dataDir: dataDir,
		port:    port,
		store:   store,
		hub:     newHub(),
		assets:  loadAssets(),
		limiter: newLimiter(),
		uploads: keyedMutex{m: map[string]*kmEntry{}},
	}
	s.noHostTrust = os.Getenv("LT_DEV_GUEST") == "1"
	go cleanIncoming(cfg.Dir)
	return s, nil
}

func (s *Server) close() { s.store.Close() }

func (s *Server) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.cfg
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /{file}", s.handleAsset)
	mux.HandleFunc("GET /api/ping", s.handlePing)
	mux.HandleFunc("POST /api/login", s.handleLogin)

	mux.HandleFunc("GET /api/sync", s.auth(s.handleSync))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /api/items", s.auth(s.handleItems))
	mux.HandleFunc("POST /api/text", s.auth(s.handleText))
	mux.HandleFunc("POST /api/upload", s.auth(s.handleUpload))
	mux.HandleFunc("POST /api/upload/cancel", s.auth(s.handleUploadCancel))
	mux.HandleFunc("POST /api/thumb", s.auth(s.handleThumb))
	mux.HandleFunc("POST /api/delete", s.auth(s.handleDelete))

	// 文件和缩略图除了 cookie，也认链接里的签名：有些安卓浏览器把下载交给
	// 系统下载器，那边不带 cookie
	mux.HandleFunc("GET /f/{id}/{name...}", s.handleFile)
	mux.HandleFunc("GET /t/{id}", s.handleThumbFile)

	mux.HandleFunc("GET /api/qr", s.hostOnly(s.handleQR))
	mux.HandleFunc("POST /api/host/open", s.hostOnly(s.handleOpen))
	mux.HandleFunc("POST /api/host/settings", s.hostOnly(s.handleSettings))
	mux.HandleFunc("POST /api/host/reset", s.hostOnly(s.handleReset))
	mux.HandleFunc("POST /api/host/clear", s.hostOnly(s.handleClear))
	return s.guard(mux)
}

// guard 给所有响应加安全头。改东西的请求必须带 X-LT 头：别的网站
// 跨域带不了自定义头（会先发预检，而这里不回 CORS），这样就挡住了 CSRF。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-LT") != "1" {
			writeErr(w, http.StatusForbidden, "bad request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 身份

// isHost 判断请求是不是来自运行本程序的电脑自己。本机打开网页不需要访问码，
// 还能用「打开文件夹」这类只在电脑上有意义的功能。
func (s *Server) isHost(r *http.Request) bool {
	if s.noHostTrust {
		return false
	}
	ip := remoteIP(r)
	if ip == nil || !(ip.IsLoopback() || s.local.has(ip)) {
		return false
	}
	// 防 DNS 重绑定：恶意网站把自己的域名解析到 127.0.0.1 再来访问的话，
	// Host 头是它的域名。这里只认 IP 和 localhost。
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil
}

func (s *Server) authed(r *http.Request) bool {
	cfg := s.config()
	if cfg.NoAuth || s.isHost(r) {
		return true
	}
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(cfg.Key)) == 1
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, "需要访问码")
			return
		}
		h(w, r)
	}
}

func (s *Server) hostOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isHost(r) {
			writeErr(w, http.StatusForbidden, "只能在运行本程序的电脑上操作")
			return
		}
		h(w, r)
	}
}

func (s *Server) setCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.config().Key,
		Path:     "/",
		MaxAge:   400 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// sign 给文件下载链接签名：拿到链接的人只能下载这一个文件
func (s *Server) sign(id string) string {
	m := hmac.New(sha256.New, []byte(s.config().Key))
	m.Write([]byte("f:" + id))
	return hex.EncodeToString(m.Sum(nil))[:24]
}

func (s *Server) sigOK(id, sig string) bool {
	return sig != "" && subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(id))) == 1
}

// pub 把记录变成发给网页的样子：去掉本机路径，加上下载签名
func (s *Server) pub(it Item) Item {
	it.Path = ""
	if it.Type == "file" {
		it.Sig = s.sign(it.ID)
	}
	return it
}

func (s *Server) pubAll(items []Item) []Item {
	for i := range items {
		items[i] = s.pub(items[i])
	}
	return items
}

func deviceOf(r *http.Request) Device {
	id := r.Header.Get("X-Device-Id")
	name := r.Header.Get("X-Device-Name")
	if id == "" {
		id = r.URL.Query().Get("did")
		name = r.URL.Query().Get("dname")
	} else if n, err := url.QueryUnescape(name); err == nil {
		name = n
	}
	if !validID(id) {
		id = "anon-" + strings.ReplaceAll(remoteIP(r).String(), ":", "-")
	}
	name = strings.TrimSpace(strings.ToValidUTF8(name, ""))
	if utf8.RuneCountInString(name) > 24 {
		name = string([]rune(name)[:24])
	}
	if name == "" {
		name = "未命名设备"
	}
	return Device{ID: id, Name: name}
}

// ---------------------------------------------------------------- 页面和静态文件

type asset struct {
	body, gz []byte
	ctype    string
	etag     string
}

func loadAssets() map[string]*asset {
	out := map[string]*asset{}
	_ = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := webFS.ReadFile(p)
		sum := sha256.Sum256(b)
		a := &asset{body: b, ctype: mimeOf(p), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		switch path.Ext(p) {
		case ".html", ".js", ".css", ".svg", ".json", ".webmanifest":
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if buf.Len() < len(b) {
				a.gz = buf.Bytes()
			}
		}
		out[strings.TrimPrefix(p, "web/")] = a
		return nil
	})
	return out
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	a := s.assets[name]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	h.Set("Vary", "Accept-Encoding")
	if strings.HasSuffix(name, ".png") {
		h.Set("Cache-Control", "public, max-age=86400")
	} else {
		h.Set("Cache-Control", "no-cache") // 每次都问一下有没有更新，局域网里一个 304 很快
	}
	if name == "index.html" {
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.body
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 扫码进来的链接带着 ?k=密钥：对了就记住这台设备，然后换成干净的地址，
	// 免得密钥留在地址栏、书签和截图里
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(s.config().Key)) == 1 {
			s.setCookie(w)
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.serveAsset(w, r, "index.html")
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if name == "favicon.ico" {
		name = "icon-192.png"
	}
	s.serveAsset(w, r, name)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"app": appID, "version": version})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r).String()
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "试错次数太多，请 10 分钟后再试")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
	code := strings.TrimSpace(body.Code)
	if subtle.ConstantTimeCompare([]byte(code), []byte(s.config().Code)) != 1 {
		s.limiter.fail(ip)
		writeErr(w, http.StatusForbidden, "访问码不对")
		return
	}
	s.limiter.reset(ip)
	s.setCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- 同步

const syncLimit = 200

func (s *Server) syncPayload(host bool) map[string]any {
	items, more := s.store.Page(0, syncLimit)
	p := map[string]any{
		"version": version,
		"me":      map[string]bool{"host": host},
		"items":   s.pubAll(items),
		"hasMore": more,
		"online":  s.hub.Online(),
	}
	if host {
		p["host"] = s.hostInfo()
	}
	return p
}

type hostURL struct {
	IP    string `json:"ip"`
	Iface string `json:"iface"`
	URL   string `json:"url"`
	Link  string `json:"link"` // 带密钥的完整链接，也就是二维码的内容
	// Virtual：虚拟机、VPN 之类的网卡，手机一般连不到，网页上默认收起来
	Virtual bool `json:"virtual,omitempty"`
}

func (s *Server) link(ip string) string {
	cfg := s.config()
	u := fmt.Sprintf("http://%s/", net.JoinHostPort(ip, strconv.Itoa(s.port)))
	if !cfg.NoAuth {
		u += "?k=" + cfg.Key
	}
	return u
}

func (s *Server) hostInfo() map[string]any {
	cfg := s.config()
	urls := []hostURL{}
	for _, a := range lanAddrs() {
		urls = append(urls, hostURL{
			IP:    a.IP,
			Iface: a.Iface,
			URL:   fmt.Sprintf("http://%s:%d", a.IP, s.port),
			Link:  s.link(a.IP),

			Virtual: a.score < 0,
		})
	}
	return map[string]any{
		"urls":      urls,
		"code":      cfg.Code,
		"dir":       cfg.Dir,
		"noAuth":    cfg.NoAuth,
		"noBrowser": cfg.NoBrowser,
		"port":      s.port,
		"repo":      repoURL,
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.syncPayload(s.isHost(r)))
}

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	items, more := s.store.Page(before, limit)
	writeJSON(w, http.StatusOK, map[string]any{"items": s.pubAll(items), "hasMore": more})
}

// handleEvents 是 Server-Sent Events 长连接。连上时先发一份完整同步，
// 所以手机锁屏断线后重连，中间漏掉的消息也会补上。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")

	host := s.isHost(r)
	dev := deviceOf(r)
	dev.Host = host
	c := s.hub.add(dev)
	defer s.hub.remove(c)

	if _, err := io.WriteString(w, "retry: 2000\n\n"); err != nil {
		return
	}
	if _, err := w.Write(sseMsg("sync", s.syncPayload(host))); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case msg := <-c.ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			// 一次把排着队的都写出去再 flush
			for n := len(c.ch); n > 0; n-- {
				if _, err := w.Write(<-c.ch); err != nil {
					return
				}
			}
		case <-ping.C:
			// 用真正的事件而不是注释：网页靠它判断连接是不是还活着
			if _, err := io.WriteString(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
		case <-c.gone:
			return
		case <-r.Context().Done():
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- 文字、缩略图、删除

func (s *Server) handleText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "格式不对")
		return
	}
	text := strings.ToValidUTF8(body.Text, "")
	if strings.TrimSpace(text) == "" {
		writeErr(w, http.StatusBadRequest, "内容是空的")
		return
	}
	if utf8.RuneCountInString(text) > 100000 {
		writeErr(w, http.StatusRequestEntityTooLarge, "文字太长了，超过 10 万字的请存成文件发送")
		return
	}
	// 网页自己生成 id：网络不好重发时，同一条消息不会出现两次
	if validID(body.ID) {
		if it, ok := s.store.Get(body.ID); ok {
			writeJSON(w, http.StatusOK, s.pub(it))
			return
		}
	} else {
		body.ID = newID()
	}
	dev := deviceOf(r)
	it := &Item{ID: body.ID, Type: "text", Text: text, From: dev.Name, FromID: dev.ID}
	s.store.Add(it)
	pub := s.pub(*it)
	s.hub.Broadcast("item", pub)
	logf("文字 · %s：%s", dev.Name, preview(text, 40))
	writeJSON(w, http.StatusOK, pub)
}

func preview(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}

func (s *Server) thumbPath(id string) string {
	return filepath.Join(s.dataDir, "thumbs", id+".jpg")
}

// handleThumb 接收发送方浏览器生成的缩略图（JPEG），顺带记下尺寸和时长
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	if !validID(id) || !s.store.Has(id) {
		writeErr(w, http.StatusNotFound, "没有这条记录")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 3<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取失败")
		return
	}
	saved := false
	if len(body) > 3 && body[0] == 0xFF && body[1] == 0xD8 {
		if err := os.WriteFile(s.thumbPath(id), body, 0o644); err == nil {
			saved = true
		}
	}
	width, _ := strconv.Atoi(q.Get("w"))
	height, _ := strconv.Atoi(q.Get("h"))
	dur, _ := strconv.ParseFloat(q.Get("dur"), 64)
	it, ok := s.store.Update(id, func(it *Item) {
		if width > 0 && height > 0 && width < 100000 && height < 100000 {
			it.W, it.H = width, height
		}
		if dur > 0 && dur < 1e7 {
			it.Dur = dur
		}
		if saved {
			it.Thumb = true
		}
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "没有这条记录")
		return
	}
	pub := s.pub(it)
	s.hub.Broadcast("item", pub)
	writeJSON(w, http.StatusOK, pub)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "格式不对")
		return
	}
	removed := s.store.Delete(body.IDs)
	s.removeFiles(removed, true)
	ids := make([]string, 0, len(removed))
	for _, it := range removed {
		ids = append(ids, it.ID)
	}
	if len(ids) > 0 {
		s.hub.Broadcast("del", map[string]any{"ids": ids})
		logf("删除了 %d 条记录 · %s", len(ids), deviceOf(r).Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
}

// removeFiles 删掉记录对应的缩略图，withFiles 时连收到的文件一起删
func (s *Server) removeFiles(items []Item, withFiles bool) {
	for _, it := range items {
		if it.Type != "file" {
			continue
		}
		_ = os.Remove(s.thumbPath(it.ID))
		if withFiles && it.Path != "" {
			_ = os.Remove(it.Path)
		}
	}
}

// ---------------------------------------------------------------- 下载

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.authed(r) && !s.sigOK(id, r.URL.Query().Get("s")) {
		writeErr(w, http.StatusUnauthorized, "需要访问码")
		return
	}
	it, ok := s.store.Get(id)
	if !ok || it.Type != "file" {
		writeErr(w, http.StatusNotFound, "这个文件已经被删除了")
		return
	}
	f, err := os.Open(it.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "电脑上找不到这个文件了，可能被移动或删除了")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, "电脑上找不到这个文件了")
		return
	}
	ctype := mimeOf(it.Name)
	disp := "attachment"
	if r.URL.Query().Get("dl") == "" && inlineOK(ctype) {
		disp = "inline"
		// iPhone 拍的 .mov 大多是 H.264/HEVC 的 MP4 封装，按 video/mp4 给浏览器更容易直接播
		if ctype == "video/quicktime" {
			ctype = "video/mp4"
		}
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", contentDisposition(disp, it.Name))
	h.Set("Cache-Control", "private, max-age=3600")
	// ServeContent 支持 Range：视频可以拖进度条，断了的下载可以续传
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) handleThumbFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) || (!s.authed(r) && !s.sigOK(id, r.URL.Query().Get("s"))) {
		writeErr(w, http.StatusUnauthorized, "需要访问码")
		return
	}
	f, err := os.Open(s.thumbPath(id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// ---------------------------------------------------------------- 只在本机能用的

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	if net.ParseIP(ip) == nil {
		ip = "127.0.0.1"
		if addrs := lanAddrs(); len(addrs) > 0 {
			ip = addrs[0].IP
		}
	}
	png, err := qrcode.Encode(s.link(ip), qrcode.Medium, 480)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID     string `json:"id"`
		Reveal bool   `json:"reveal"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	var err error
	if body.ID == "" {
		err = shellOpen(s.config().Dir)
	} else {
		it, ok := s.store.Get(body.ID)
		if !ok || it.Type != "file" {
			writeErr(w, http.StatusNotFound, "没有这个文件")
			return
		}
		if _, e := os.Stat(it.Path); e != nil {
			writeErr(w, http.StatusNotFound, "电脑上找不到这个文件了，可能被移动或删除了")
			return
		}
		// 程序、脚本一类的文件不直接运行，只在文件夹里选中它
		if body.Reveal || isRisky(it.Name) {
			err = revealFile(it.Path)
		} else {
			err = shellOpen(it.Path)
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打不开："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir       *string `json:"dir"`
		NoAuth    *bool   `json:"noAuth"`
		NoBrowser *bool   `json:"noBrowser"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "格式不对")
		return
	}
	var dir string
	if body.Dir != nil {
		d, err := checkDir(*body.Dir)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		dir = d
	}
	s.mu.Lock()
	if dir != "" {
		s.cfg.Dir = dir
	}
	if body.NoAuth != nil {
		s.cfg.NoAuth = *body.NoAuth
	}
	if body.NoBrowser != nil {
		s.cfg.NoBrowser = *body.NoBrowser
	}
	err := s.cfg.save()
	s.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
		return
	}
	if dir != "" {
		logf("接收文件夹改成了 %s", dir)
	}
	info := s.hostInfo()
	s.hub.BroadcastHost("host", info)
	writeJSON(w, http.StatusOK, info)
}

// checkDir 确认文件夹能用：是绝对路径、能创建、能写入
func checkDir(d string) (string, error) {
	d = strings.Trim(strings.TrimSpace(d), `"'`)
	if strings.HasPrefix(d, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			d = filepath.Join(home, d[1:])
		}
	}
	if d == "" || !filepath.IsAbs(d) {
		return "", fmt.Errorf("请填写完整的文件夹路径，比如 D:\\收到的文件")
	}
	d = filepath.Clean(d)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("没法创建这个文件夹：%v", err)
	}
	f, err := os.CreateTemp(d, ".lt-test-*")
	if err != nil {
		return "", fmt.Errorf("这个文件夹没法写入：%v", err)
	}
	f.Close()
	os.Remove(f.Name())
	return d, nil
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.cfg.Key = randHex(16)
	s.cfg.Code = randCode()
	err := s.cfg.save()
	s.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
		return
	}
	s.hub.KickGuests()
	logf("访问码换成了 %s，其他设备需要重新扫码", s.config().Code)
	info := s.hostInfo()
	s.hub.BroadcastHost("host", info)
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Files bool `json:"files"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
	removed := s.store.Clear()
	s.removeFiles(removed, body.Files)
	ids := make([]string, 0, len(removed))
	for _, it := range removed {
		ids = append(ids, it.ID)
	}
	s.hub.Broadcast("del", map[string]any{"ids": ids})
	if body.Files {
		logf("清空了聊天记录和收到的文件")
	} else {
		logf("清空了聊天记录（文件还留在文件夹里）")
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": len(ids)})
}

// ---------------------------------------------------------------- 工具

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return net.IPv4zero
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// localIPs 缓存本机所有网卡的地址，用来认出「电脑自己用局域网 IP 打开的网页」
type localIPs struct {
	mu  sync.Mutex
	at  time.Time
	set map[string]bool
}

func (l *localIPs) has(ip net.IP) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.at) > 10*time.Second {
		l.set = map[string]bool{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					l.set[normIP(n.IP)] = true
				}
			}
		}
		l.at = time.Now()
	}
	return l.set[normIP(ip)]
}

func normIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

// limiter 限制输错访问码的次数：同一个 IP 连错 5 次锁 10 分钟
type limiter struct {
	mu sync.Mutex
	m  map[string]*failRec
}

type failRec struct {
	n     int
	until time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*failRec{}} }

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.m[ip]
	return f == nil || time.Now().After(f.until)
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.m[ip]
	if f == nil {
		f = &failRec{}
		l.m[ip] = f
	}
	f.n++
	if f.n >= 5 {
		f.n = 0
		f.until = time.Now().Add(10 * time.Minute)
	}
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

// keyedMutex 是按 key 分开的锁：同一个上传的分块一个一个写，不同上传互不影响
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*kmEntry
}

type kmEntry struct {
	mu sync.Mutex
	n  int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	e := k.m[key]
	if e == nil {
		e = &kmEntry{}
		k.m[key] = e
	}
	e.n++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.n--
		if e.n == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
