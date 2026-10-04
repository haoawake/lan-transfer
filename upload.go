package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 上传协议（为了快和稳）：
//
//	POST /api/upload?id=&name=&size=&offset=   请求体就是文件从 offset 开始的一段原始字节
//	GET  /api/upload?id=                         问电脑上已经收到了多少：{received} 或 {done, item}
//
// 网页把文件切成 16 MB 一块按顺序发，服务端直接追加写进 <接收文件夹>/.incoming/<id>.part，
// 不经过 multipart 解析，也不在内存里攒数据。每块都带着完整的文件信息，所以服务端
// 不用记任何上传状态，程序重启后也能接着传。收满 size 字节就改名成正式文件。
//
// 断线续传的几个坑（v1.0.0 都踩了）：
//   - 一块传到一半断了，电脑上已经有这块的前半截。网页从这块开头重发时，重复的部分
//     直接跳过，不能回 409 然后断开——浏览器还在往上传数据时连接被断，它只会看到
//     「网络错误」，读不到 409，于是永远从同一个位置重发，传输彻底卡死。
//   - 任何时候要提前回应（409、硬盘满了），都先把这块剩下的请求体读完，浏览器才收得到。
//   - 手机锁屏、切后台时，旧连接可能半死不活地挂着，占着这个上传的锁。新请求一到就把
//     旧的掐掉；30 秒一个字节都没收到的连接也直接放弃，不能等 TCP 保活的好几分钟。

const (
	incomingDir = ".incoming"
	diskReserve = 100 << 20 // 至少给硬盘留 100 MB，别把系统盘写满
	maxDrain    = 64 << 20  // 提前回应前最多读掉这么多请求体（网页每块 16 MB）
)

// uploadIdle：这么久一个字节都没收到，就当这个连接已经死了（测试里会改短）
var uploadIdle = 30 * time.Second

// freeSpace 查磁盘剩余空间，测试里会替换掉
var freeSpace = diskFree

var errSuperseded = errors.New("被同一个上传的新请求顶替了")

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

var claimGen atomic.Uint64

// uploadClaim 记着某个上传当前是哪个请求在处理
type uploadClaim struct {
	stopped atomic.Bool
	rc      *http.ResponseController
	gen     uint64       // 每个请求一个编号
	rx      atomic.Int64 // 这个请求已经从网络上读到多少字节；网页靠它判断连接是不是还活着
}

func (c *uploadClaim) stop() {
	c.stopped.Store(true)
	if c.rc != nil {
		_ = c.rc.SetReadDeadline(time.Now()) // 让正卡在读网络上的旧请求马上返回
	}
}

// claim 宣布由这个请求处理上传 id：还挂着的旧请求立刻被掐掉
func (s *Server) claim(id string, rc *http.ResponseController) (*uploadClaim, func()) {
	c := &uploadClaim{rc: rc, gen: claimGen.Add(1)}
	s.claimMu.Lock()
	if old := s.claims[id]; old != nil {
		old.stop()
	}
	s.claims[id] = c
	s.claimMu.Unlock()
	return c, func() {
		s.claimMu.Lock()
		if s.claims[id] == c {
			delete(s.claims, id)
		}
		s.claimMu.Unlock()
	}
}

// idleReader 每次读之前把读超时往后推 uploadIdle；被新请求顶替后立刻失败
type idleReader struct {
	r io.Reader
	c *uploadClaim
}

func (ir idleReader) Read(p []byte) (int, error) {
	if ir.c.stopped.Load() {
		return 0, errSuperseded
	}
	if ir.c.rc != nil {
		_ = ir.c.rc.SetReadDeadline(time.Now().Add(uploadIdle))
	}
	if ir.c.stopped.Load() { // stop() 可能刚好发生在上面两步之间
		return 0, errSuperseded
	}
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.c.rx.Add(int64(n))
	}
	if err != nil && ir.c.stopped.Load() {
		err = errSuperseded
	}
	return n, err
}

// upStat 记着一个上传从开始到传完的情况，传完时打印在控制台里，方便看传输顺不顺
type upStat struct {
	start   time.Time
	name    string
	resumes int // 中途卡住、换新连接接着传的次数
}

func (s *Server) upStat(id, name string) *upStat {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	st := s.stats[id]
	if st == nil {
		for k, v := range s.stats { // 顺手清掉一天前没传完的
			if time.Since(v.start) > 24*time.Hour {
				delete(s.stats, k)
			}
		}
		st = &upStat{start: time.Now()}
		s.stats[id] = st
	}
	if name != "" {
		st.name = name
	}
	return st
}

func (s *Server) dropStat(id string) *upStat {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	st := s.stats[id]
	delete(s.stats, id)
	return st
}

// drain 读掉请求体里剩下的部分。提前回应前必须这样做，浏览器才收得到回应
func drain(r io.Reader) { _, _ = io.CopyN(io.Discard, r, maxDrain) }

func (s *Server) partPath(dir, id string) string { return filepath.Join(dir, incomingDir, id+".part") }

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	size, err1 := strconv.ParseInt(q.Get("size"), 10, 64)
	offset, err2 := strconv.ParseInt(q.Get("offset"), 10, 64)
	if !validID(id) || err1 != nil || err2 != nil || size < 0 || offset < 0 || offset > size {
		writeErr(w, http.StatusBadRequest, "参数不对")
		return
	}
	name := sanitizeName(q.Get("name"))
	dev := deviceOf(r)
	s.upStat(id, name)

	c, release := s.claim(id, http.NewResponseController(w))
	defer release()
	body := idleReader{r: r.Body, c: c}
	unlock := s.uploads.lock(id)
	defer unlock()

	// 已经传完了（比如上一块的响应在路上丢了，网页又重发）：直接告诉它完成了
	if it, ok := s.store.Get(id); ok {
		drain(body)
		s.dropStat(id)
		writeJSON(w, http.StatusOK, map[string]any{"done": true, "item": s.pub(it)})
		return
	}

	dir := s.config().Dir
	partDir := filepath.Join(dir, incomingDir)
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		drain(body)
		writeErr(w, http.StatusInternalServerError, "没法写入接收文件夹："+err.Error())
		return
	}
	hideFile(partDir)

	f, err := os.OpenFile(s.partPath(dir, id), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		drain(body)
		writeErr(w, http.StatusInternalServerError, "没法写入接收文件夹："+err.Error())
		return
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		drain(body)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	have := st.Size()
	if have > size { // 正常不会出现，大概是同一个 id 换了文件：从头来
		_ = f.Truncate(0)
		have = 0
	}
	if offset > have {
		// 网页以为电脑上的比实际多（比如中途换了接收文件夹）：告诉它从哪里重新发
		f.Close()
		drain(body)
		writeJSON(w, http.StatusConflict, map[string]int64{"received": have})
		return
	}
	if skip := have - offset; skip > 0 {
		// 网页重发了电脑上已经有的部分（上一块传到一半断了）：跳过重复的字节，接着往后写
		if _, err := io.CopyN(io.Discard, body, skip); err != nil {
			f.Close()
			writeJSON(w, http.StatusOK, map[string]int64{"received": have})
			return
		}
	}
	if free, err := freeSpace(dir); err == nil && free < uint64(size-have)+diskReserve {
		f.Close()
		drain(body)
		writeErr(w, http.StatusInsufficientStorage, fullMsg(free, size))
		return
	}
	if _, err := f.Seek(have, io.SeekStart); err != nil {
		f.Close()
		drain(body)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	bp := copyBufPool.Get().(*[]byte)
	n, werr, rerr := copyChunk(f, body, size-have, *bp)
	copyBufPool.Put(bp)
	have += n
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		drain(body)
		if isDiskFull(werr) {
			free, _ := freeSpace(dir)
			writeErr(w, http.StatusInsufficientStorage, fullMsg(free, size))
		} else {
			writeErr(w, http.StatusInternalServerError, "写入失败："+werr.Error())
		}
		return
	}
	if have < size {
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			// 连接断了或被新请求顶替了，网页大概已经收不到了；它会先问清楚收到多少再接着发
			writeErr(w, http.StatusRequestTimeout, "连接中断")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"received": have})
		return
	}

	// 收齐了：改名成正式文件，名字重复就加 (1) (2)
	s.finalMu.Lock()
	final := uniquePath(dir, name)
	err = os.Rename(s.partPath(dir, id), final)
	s.finalMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存文件失败："+err.Error())
		return
	}
	it := &Item{
		ID:     id,
		Type:   "file",
		Name:   name,
		Size:   size,
		Mime:   mimeOf(name),
		Path:   final,
		From:   dev.Name,
		FromID: dev.ID,
	}
	s.store.Add(it)
	pub := s.pub(*it)
	s.hub.Broadcast("item", pub)
	logf("文件 · %s：%s（%s%s）", dev.Name, name, humanSize(size), statSummary(s.dropStat(id), size))
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "item": pub})
}

// statSummary：大文件传完时附上用时、平均速度和中途卡住的次数
func statSummary(st *upStat, size int64) string {
	if st == nil || size < 16<<20 {
		return ""
	}
	d := time.Since(st.start)
	out := fmt.Sprintf("，用时 %s，平均 %s/s", humanDuration(d), humanSize(int64(float64(size)/d.Seconds())))
	if st.resumes > 0 {
		out += fmt.Sprintf("，中途卡住 %d 次", st.resumes)
	}
	return out
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1f 秒", d.Seconds())
	}
	return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
}

func fullMsg(free uint64, size int64) string {
	return fmt.Sprintf("电脑硬盘只剩 %s 了，放不下这个 %s 的文件。请在电脑上的网页里点「···」，把接收文件夹换到空间大的盘。",
		humanSize(int64(free)), humanSize(size))
}

// handleUploadStatus 告诉网页电脑上已经收到了多少。网页断线后先问这个，再从这里接着发，
// 不用把已经传过的部分再发一遍。顺带把还挂着的旧请求掐掉。
//
// 带 peek=1 时只是看一眼：不掐旧请求，返回正在处理这个上传的请求编号和它已经读到的字节数。
// 网页好几秒没看到上传进度时用它判断——数据还在往电脑上走（只是浏览器没报进度），
// 还是连接真的卡死了、该换个新连接。
func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validID(id) {
		writeErr(w, http.StatusBadRequest, "参数不对")
		return
	}
	if r.URL.Query().Get("peek") == "1" {
		s.claimMu.Lock()
		c := s.claims[id]
		s.claimMu.Unlock()
		if c == nil || c.rc == nil {
			writeJSON(w, http.StatusOK, map[string]any{"active": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active": true, "gen": c.gen, "rx": c.rx.Load()})
		return
	}

	_, release := s.claim(id, nil)
	defer release()
	unlock := s.uploads.lock(id)
	defer unlock()
	if it, ok := s.store.Get(id); ok {
		writeJSON(w, http.StatusOK, map[string]any{"done": true, "item": s.pub(it)})
		return
	}
	var have int64
	if st, err := os.Stat(s.partPath(s.config().Dir, id)); err == nil {
		have = st.Size()
	}
	// 网页只在断线、卡住之后才来问进度，所以每问一次就是卡了一次
	if st := s.upStat(id, ""); st != nil {
		st.resumes++
		logf("卡住了一下 · %s：%s 从 %s 处接着传（第 %d 次）", deviceOf(r).Name, st.name, humanSize(have), st.resumes)
	}
	writeJSON(w, http.StatusOK, map[string]int64{"received": have})
}

// copyChunk 把请求体里最多 n 个字节写进文件。每次攒满 1 MB 再写，
// 比按网络包大小零碎地写快得多。写盘错误和读网络错误分开返回。
func copyChunk(f *os.File, r io.Reader, n int64, buf []byte) (written int64, werr, rerr error) {
	for written < n {
		want := int64(len(buf))
		if rest := n - written; rest < want {
			want = rest
		}
		m, err := io.ReadFull(r, buf[:want])
		if m > 0 {
			if _, e := f.Write(buf[:m]); e != nil {
				return written, e, nil
			}
			written += int64(m)
		}
		if err != nil {
			return written, nil, err
		}
	}
	return written, nil, nil
}

func (s *Server) handleUploadCancel(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !validID(id) {
		writeErr(w, http.StatusBadRequest, "参数不对")
		return
	}
	_, release := s.claim(id, nil)
	unlock := s.uploads.lock(id)
	_ = os.Remove(s.partPath(s.config().Dir, id))
	s.dropStat(id)
	unlock()
	release()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// cleanIncoming 删掉三天前没传完的半截文件
func cleanIncoming(dir string) {
	partDir := filepath.Join(dir, incomingDir)
	entries, err := os.ReadDir(partDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 72*time.Hour {
			_ = os.Remove(filepath.Join(partDir, e.Name()))
		}
	}
}
