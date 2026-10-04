package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// Config 存在 <数据文件夹>/config.json，手动改了重启程序就生效
type Config struct {
	Port int    `json:"port"`
	Dir  string `json:"dir"` // 接收文件夹
	// Key 放在二维码链接里，扫码即登录；Code 是手动输入地址时要填的 6 位访问码。
	// 两个都只是「同一个 Wi-Fi 里的别人别随便进来」，不是什么高强度的密码。
	Key       string `json:"key"`
	Code      string `json:"code"`
	NoAuth    bool   `json:"noAuth"`    // 不要访问码（只建议在自己家里的网络用）
	NoBrowser bool   `json:"noBrowser"` // 启动时不自动打开浏览器

	path string
}

func loadConfig(dataDir string) (*Config, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	c := &Config{path: filepath.Join(dataDir, "config.json")}
	b, err := os.ReadFile(c.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s 格式不对：%v", c.path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	changed := false
	if c.Port <= 0 || c.Port > 65535 {
		c.Port, changed = defaultPort, true
	}
	if c.Dir == "" {
		c.Dir, changed = filepath.Join(downloadsDir(), appName), true
	}
	if len(c.Key) < 16 {
		c.Key, changed = randHex(16), true
	}
	if !validCode(c.Code) {
		c.Code, changed = randCode(), true
	}
	if changed {
		if err := c.save(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Config) save() error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(c.path, b)
}

func validCode(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "000000"
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// writeFileAtomic 先写临时文件再改名，写到一半断电也不会把原文件弄坏
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
