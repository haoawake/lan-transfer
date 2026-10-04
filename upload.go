package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 上传协议（为了快和稳）：
//
//	POST /api/upload?id=&name=&size=&offset=   请求体就是文件的这一段原始字节
//
// 网页把文件切成 16 MB 一块按顺序发，服务端直接追加写进 <接收文件夹>/.incoming/<id>.part，
// 不经过 multipart 解析，也不在内存里攒数据。每块都带着完整的文件信息，所以服务端
// 不用记任何上传状态，程序重启后也能接着传。
//
// offset 和服务端已有的字节数对不上时回 409 + {received}，网页从那里接着发——
// 手机锁屏、Wi-Fi 抖一下都只用重传断掉的那一块。收满 size 字节就改名成正式文件。

const incomingDir = ".incoming"

var copyBufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

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

	unlock := s.uploads.lock(id)
	defer unlock()

	// 已经传完了（比如上一块的响应在路上丢了，网页又重发）：直接告诉它完成了
	if it, ok := s.store.Get(id); ok {
		writeJSON(w, http.StatusOK, map[string]any{"done": true, "item": s.pub(it)})
		return
	}

	dir := s.config().Dir
	partDir := filepath.Join(dir, incomingDir)
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "没法写入接收文件夹："+err.Error())
		return
	}
	hideFile(partDir)
	part := filepath.Join(partDir, id+".part")

	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "没法写入接收文件夹："+err.Error())
		return
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	have := st.Size()
	if have > size { // 不可能正常出现，大概是同一个 id 换了文件：从头来
		f.Truncate(0)
		have = 0
	}
	if offset != have {
		f.Close()
		writeJSON(w, http.StatusConflict, map[string]int64{"received": have})
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	bp := copyBufPool.Get().(*[]byte)
	n, werr, rerr := copyChunk(f, r.Body, size-offset, *bp)
	copyBufPool.Put(bp)
	have += n
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		if isDiskFull(werr) {
			writeErr(w, http.StatusInsufficientStorage, "电脑硬盘空间不够了")
		} else {
			writeErr(w, http.StatusInternalServerError, "写入失败："+werr.Error())
		}
		return
	}
	if have < size {
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			// 连接断了，网页大概已经收不到了；它重试时会通过 409 拿到正确的位置
			writeErr(w, http.StatusBadRequest, "连接中断")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"received": have})
		return
	}

	// 收齐了：改名成正式文件，名字重复就加 (1) (2)
	s.finalMu.Lock()
	final := uniquePath(dir, name)
	err = os.Rename(part, final)
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
	logf("文件 · %s：%s（%s）", dev.Name, name, humanSize(size))
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "item": pub})
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
	unlock := s.uploads.lock(id)
	_ = os.Remove(filepath.Join(s.config().Dir, incomingDir, id+".part"))
	unlock()
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
