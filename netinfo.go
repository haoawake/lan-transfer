package main

import (
	"bytes"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Addr 是一个能让手机访问到的本机地址
type Addr struct {
	IP    string
	Iface string
	score int
}

// 虚拟网卡、VPN、代理软件的网卡：手机一般连不到，排到后面
var virtualHints = []string{
	"vmware", "virtualbox", "vbox", "vethernet", "hyper-v", "wsl", "docker", "veth", "virbr", "br-",
	"tailscale", "zerotier", "utun", "tun", "tap", "vpn", "clash", "mihomo", "wireguard", "wintun",
	"radmin", "hamachi", "loopback", "bluetooth", "蓝牙", "npcap", "awdl", "llw", "bridge", "anpi",
}

// 常见的真实网卡名
var physicalHints = []string{"wi-fi", "wifi", "wlan", "wireless", "以太网", "ethernet", "en0", "en1", "eth", "wlp", "enp", "wlo", "eno"}

// 虚拟机网卡的 MAC 前缀
var virtualMACs = [][]byte{
	{0x00, 0x50, 0x56}, {0x00, 0x0c, 0x29}, {0x00, 0x05, 0x69}, // VMware
	{0x08, 0x00, 0x27}, {0x0a, 0x00, 0x27}, // VirtualBox
	{0x00, 0x15, 0x5d}, // Hyper-V / WSL
	{0x00, 0x1c, 0x42}, // Parallels
	{0x02, 0x42},       // Docker
}

var addrCache struct {
	sync.Mutex
	at    time.Time
	addrs []Addr
}

// lanAddrs 列出本机的局域网 IPv4 地址，最可能被手机访问到的排在最前面。
// 二维码默认用第一个；电脑上有多张网卡时，网页上可以切换。
func lanAddrs() []Addr {
	addrCache.Lock()
	defer addrCache.Unlock()
	if time.Since(addrCache.at) < 5*time.Second && addrCache.addrs != nil {
		return addrCache.addrs
	}
	route := defaultRouteIP()
	out := []Addr{}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := n.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			// 198.18.0.0/15 是 Clash、Surge 一类代理软件的虚拟地址
			if ip[0] == 198 && (ip[1] == 18 || ip[1] == 19) {
				continue
			}
			out = append(out, Addr{IP: ip.String(), Iface: ifc.Name, score: scoreAddr(ip, ifc, route)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	addrCache.addrs, addrCache.at = out, time.Now()
	return out
}

func scoreAddr(ip net.IP, ifc net.Interface, route net.IP) int {
	s := 0
	switch {
	case ip[0] == 192 && ip[1] == 168:
		s = 30
	case ip[0] == 10, ip[0] == 172 && ip[1] >= 16 && ip[1] < 32:
		s = 20
	case ip[0] == 100 && ip[1] >= 64 && ip[1] < 128: // 运营商级 NAT / Tailscale
		s = 5
	default:
		s = 10
	}
	name := strings.ToLower(ifc.Name)
	for _, h := range virtualHints {
		if strings.Contains(name, h) {
			s -= 50
			break
		}
	}
	for _, h := range physicalHints {
		if strings.Contains(name, h) {
			s += 5
			break
		}
	}
	for _, p := range virtualMACs {
		if len(ifc.HardwareAddr) >= len(p) && bytes.Equal(ifc.HardwareAddr[:len(p)], p) {
			s -= 40
			break
		}
	}
	if route != nil && route.Equal(ip) {
		s += 15
	}
	return s
}

// defaultRouteIP 找出访问外网时用的本机地址（UDP 「连接」不会真的发包）
func defaultRouteIP() net.IP {
	c, err := net.DialTimeout("udp4", "223.5.5.5:53", time.Second)
	if err != nil {
		return nil
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.To4()
	}
	return nil
}
