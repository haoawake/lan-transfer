package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func validID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeName 把网页传来的文件名变成在 Windows / macOS / Linux 上都安全的名字：
// 去掉路径、非法字符、Windows 保留名，限制长度。
func sanitizeName(name string) string {
	name = strings.ToValidUTF8(name, "_")
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 32 || r == 127:
			// 控制字符直接丢掉
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimSpace(b.String())
	name = strings.TrimRight(name, ". ") // Windows 不允许以点或空格结尾
	if name == "" {
		name = "未命名文件"
	}
	ext := filepath.Ext(name)
	if reservedNames[strings.ToUpper(strings.TrimSuffix(name, ext))] {
		name = "_" + name
	}
	// 大多数文件系统限制 255 字节，留点余量给 " (12)" 这样的后缀
	const maxBytes = 200
	if len(name) > maxBytes {
		if len(ext) > 20 || !utf8.ValidString(ext) {
			ext = ""
		}
		stem := strings.TrimSuffix(name, ext)
		for len(stem)+len(ext) > maxBytes {
			_, size := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-size]
		}
		name = stem + ext
	}
	return name
}

// uniquePath 在 dir 里给 name 找一个没被占用的路径：a.jpg、a (1).jpg、a (2).jpg…
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 100000; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p
		}
	}
	return filepath.Join(dir, newID()+"-"+name)
}

// 自带一张常见类型表，不读系统注册表（Windows 上注册表里的类型经常被别的软件改坏）
var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
	".json": "application/json", ".webmanifest": "application/manifest+json",
	".txt": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".xml": "application/xml",

	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".heic": "image/heic", ".heif": "image/heif",
	".avif": "image/avif", ".svg": "image/svg+xml", ".ico": "image/x-icon", ".tif": "image/tiff", ".tiff": "image/tiff",

	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
	".mkv": "video/x-matroska", ".avi": "video/x-msvideo", ".3gp": "video/3gpp", ".flv": "video/x-flv",
	".wmv": "video/x-ms-wmv", ".ts": "video/mp2t", ".mts": "video/mp2t", ".m2ts": "video/mp2t",

	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".wav": "audio/wav",
	".flac": "audio/flac", ".ogg": "audio/ogg", ".opus": "audio/ogg", ".amr": "audio/amr", ".wma": "audio/x-ms-wma",

	".pdf": "application/pdf", ".zip": "application/zip", ".rar": "application/vnd.rar",
	".7z": "application/x-7z-compressed", ".gz": "application/gzip", ".tar": "application/x-tar",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".apk": "application/vnd.android.package-archive", ".epub": "application/epub+zip",
}

func mimeOf(name string) string {
	if t, ok := mimeTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

// inlineOK：这些类型可以直接在浏览器里看。其余的（尤其 HTML、SVG）一律当附件下载，
// 免得收到的网页文件在本站的域名下运行脚本。
func inlineOK(ctype string) bool {
	if ctype == "image/svg+xml" {
		return false
	}
	return strings.HasPrefix(ctype, "image/") || strings.HasPrefix(ctype, "video/") ||
		strings.HasPrefix(ctype, "audio/") || ctype == "application/pdf"
}

// contentDisposition 同时给出 ASCII 的 filename 和 UTF-8 的 filename*，
// 中文文件名在新老浏览器里都能正确保存
func contentDisposition(disp, name string) string {
	var ascii strings.Builder
	for _, r := range name {
		if r < 32 || r > 126 || r == '"' || r == '\\' || r == '%' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(r)
		}
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disp, ascii.String(), rfc5987(name))
}

func rfc5987(s string) string {
	const ok = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(ok, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// isRisky：在电脑上点「打开」时不直接运行这些文件，只在文件夹里选中
func isRisky(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".msi", ".bat", ".cmd", ".com", ".scr", ".ps1", ".vbs", ".vbe", ".js", ".jse", ".wsf", ".wsh",
		".lnk", ".url", ".reg", ".hta", ".cpl", ".msc", ".jar", ".pif", ".appref-ms", ".sh", ".command", ".app", ".pkg":
		return true
	}
	return false
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
