package main

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

// Item 是聊天记录里的一条：一段文字或者一个文件
type Item struct {
	ID     string  `json:"id"`
	Type   string  `json:"type"` // "text" 或 "file"
	Text   string  `json:"text,omitempty"`
	Name   string  `json:"name,omitempty"`
	Size   int64   `json:"size,omitempty"`
	Mime   string  `json:"mime,omitempty"`
	Thumb  bool    `json:"thumb,omitempty"` // 有没有缩略图（由发送方的浏览器生成后上传）
	W      int     `json:"w,omitempty"`     // 图片 / 视频的原始尺寸，网页用来提前占好位置
	H      int     `json:"h,omitempty"`
	Dur    float64 `json:"dur,omitempty"` // 视频时长（秒）
	From   string  `json:"from"`
	FromID string  `json:"fromId"`
	Time   int64   `json:"time"` // 毫秒时间戳，同时也是排序依据，保证严格递增

	Path string `json:"path,omitempty"` // 文件在电脑上的位置，只存在本机，不发给网页
	Sig  string `json:"sig,omitempty"`  // 下载链接的签名，发给网页时才填
}

// Store 是全部聊天记录，按时间排好序放在内存里，有改动就写回 history.json
type Store struct {
	mu    sync.RWMutex
	items []*Item
	byID  map[string]*Item
	file  string
	dirty chan struct{}
	done  chan struct{}
}

func openStore(file string) (*Store, error) {
	s := &Store{byID: map[string]*Item{}, file: file, dirty: make(chan struct{}, 1), done: make(chan struct{})}
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		var items []*Item
		if err := json.Unmarshal(b, &items); err != nil {
			// 记录坏了不至于让程序起不来：留一份备份，从空的开始
			_ = os.Rename(file, file+".broken")
		}
		pruned := false
		for _, it := range items {
			if it == nil || it.ID == "" || s.byID[it.ID] != nil {
				continue
			}
			// 用户在文件夹里手动删掉的文件，记录也跟着去掉
			if it.Type == "file" {
				if _, err := os.Stat(it.Path); err != nil {
					pruned = true
					continue
				}
			}
			s.items = append(s.items, it)
			s.byID[it.ID] = it
		}
		sort.SliceStable(s.items, func(i, j int) bool { return s.items[i].Time < s.items[j].Time })
		if pruned {
			s.markDirty()
		}
	}
	go s.saveLoop()
	return s, nil
}

func (s *Store) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Store) saveLoop() {
	for {
		select {
		case <-s.dirty:
			time.Sleep(300 * time.Millisecond) // 一批文件陆续到达时合并成一次写盘
			s.save()
		case <-s.done:
			return
		}
	}
}

func (s *Store) save() {
	s.mu.RLock()
	b, err := json.Marshal(s.items)
	s.mu.RUnlock()
	if err == nil {
		if err := writeFileAtomic(s.file, b); err != nil {
			logf("保存聊天记录失败：%v", err)
		}
	}
}

// Close 立即把没写盘的改动写下去
func (s *Store) Close() {
	close(s.done)
	s.save()
}

func (s *Store) Add(it *Item) {
	s.mu.Lock()
	now := time.Now().UnixMilli()
	if n := len(s.items); n > 0 && now <= s.items[n-1].Time {
		now = s.items[n-1].Time + 1
	}
	it.Time = now
	s.items = append(s.items, it)
	s.byID[it.ID] = it
	s.mu.Unlock()
	s.markDirty()
}

func (s *Store) Get(id string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.byID[id]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

func (s *Store) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id] != nil
}

// Update 在锁里修改一条记录，返回修改后的副本
func (s *Store) Update(id string, fn func(*Item)) (Item, bool) {
	s.mu.Lock()
	it, ok := s.byID[id]
	if ok {
		fn(it)
	}
	var out Item
	if ok {
		out = *it
	}
	s.mu.Unlock()
	if ok {
		s.markDirty()
	}
	return out, ok
}

// Page 返回早于 before（毫秒，0 表示不限）的最新 limit 条，以及更早的是否还有
func (s *Store) Page(before int64, limit int) ([]Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	end := len(s.items)
	if before > 0 {
		end = sort.Search(len(s.items), func(i int) bool { return s.items[i].Time >= before })
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	out := make([]Item, 0, end-start)
	for _, it := range s.items[start:end] {
		out = append(out, *it)
	}
	return out, start > 0
}

func (s *Store) Delete(ids []string) []Item {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	var removed []Item
	kept := s.items[:0]
	for _, it := range s.items {
		if want[it.ID] {
			removed = append(removed, *it)
			delete(s.byID, it.ID)
		} else {
			kept = append(kept, it)
		}
	}
	for i := len(kept); i < len(s.items); i++ {
		s.items[i] = nil
	}
	s.items = kept
	s.mu.Unlock()
	if len(removed) > 0 {
		s.markDirty()
	}
	return removed
}

func (s *Store) Clear() []Item {
	s.mu.Lock()
	removed := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		removed = append(removed, *it)
	}
	s.items = nil
	s.byID = map[string]*Item{}
	s.mu.Unlock()
	s.markDirty()
	return removed
}
