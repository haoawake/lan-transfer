package main

import (
	"encoding/json"
	"sync"
)

// Device 是一个打开着网页的设备
type Device struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host bool   `json:"host"` // 就是运行本程序的这台电脑
}

// Hub 管理所有 SSE 连接，把新消息、删除、在线设备的变化实时推给每个网页
type Hub struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
}

type sseClient struct {
	ch   chan []byte
	dev  Device
	gone chan struct{}
	once sync.Once
}

func (c *sseClient) kick() { c.once.Do(func() { close(c.gone) }) }

func newHub() *Hub { return &Hub{clients: map[*sseClient]struct{}{}} }

func sseMsg(event string, v any) []byte {
	b, _ := json.Marshal(v)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}

func (h *Hub) add(dev Device) *sseClient {
	c := &sseClient{ch: make(chan []byte, 256), dev: dev, gone: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.broadcastOnline()
	return c
}

func (h *Hub) remove(c *sseClient) {
	h.mu.Lock()
	_, ok := h.clients[c]
	delete(h.clients, c)
	h.mu.Unlock()
	c.kick()
	if ok {
		h.broadcastOnline()
	}
}

// send 不等待：哪个网页卡住了收不动，就把它踢掉，它重连后会拿到一份完整的同步
func (h *Hub) send(msg []byte, onlyHost bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if onlyHost && !c.dev.Host {
			continue
		}
		select {
		case c.ch <- msg:
		default:
			c.kick()
		}
	}
}

func (h *Hub) Broadcast(event string, v any) { h.send(sseMsg(event, v), false) }

func (h *Hub) BroadcastHost(event string, v any) { h.send(sseMsg(event, v), true) }

// Online 返回在线设备，同一台设备开了几个页面只算一次
func (h *Hub) Online() []Device {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	out := []Device{}
	for c := range h.clients {
		if seen[c.dev.ID] {
			continue
		}
		seen[c.dev.ID] = true
		out = append(out, c.dev)
	}
	return out
}

func (h *Hub) broadcastOnline() { h.Broadcast("online", h.Online()) }

// KickGuests 断开除本机以外的所有设备（换了访问码之后用）
func (h *Hub) KickGuests() {
	h.mu.Lock()
	for c := range h.clients {
		if !c.dev.Host {
			c.kick()
		}
	}
	h.mu.Unlock()
}
