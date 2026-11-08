// Package hostaddr picks the LAN address that the speaker's renderer, and the
// hardware presets it stores, should use to reach this daemon.
//
// The trap it exists to avoid: on a cold boot the firmware API on 127.0.0.1
// answers before Wi-Fi has a DHCP lease, and the Bose firmware has already
// brought up an internal usb0 gadget link at 203.0.113.1/30. Picking "the first
// interface with an address" at that moment advertises usb0 — which happens to
// work for on-device playback, but gets persisted into the speaker's presets
// and breaks if that link is ever absent.
package hostaddr

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// docNets are the RFC 5737 documentation ranges. They are never a real LAN
// address; Bose uses 203.0.113.1/30 for its internal usb0 link.
var docNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// Usable reports whether ip, found on the named interface, may be advertised to
// the renderer and to other devices on the LAN.
func Usable(ifName string, ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() || v4.IsLinkLocalUnicast() {
		return false
	}
	if strings.HasPrefix(ifName, "usb") {
		return false // Bose's internal USB-ethernet gadget link
	}
	for _, n := range docNets {
		if n.Contains(v4) {
			return false
		}
	}
	return true
}

// Iface is the part of a network interface the choice depends on.
type Iface struct {
	Name  string
	Up    bool
	Addrs []net.IP
}

// Finder looks up candidate addresses. Both funcs are injectable so the choice
// can be tested without a network.
type Finder struct {
	// RouteSource returns the local address the kernel would use to reach
	// target. It must fail when there is no route; no packets are sent.
	RouteSource func(target string) (net.IP, error)
	Interfaces  func() ([]Iface, error)
}

// System returns a Finder backed by the real network stack.
func System() Finder {
	return Finder{
		RouteSource: func(target string) (net.IP, error) {
			// A UDP "dial" only consults the routing table; it sends nothing.
			// It fails with "network is unreachable" until a route exists.
			c, err := net.DialTimeout("udp", target, 2*time.Second)
			if err != nil {
				return nil, err
			}
			defer c.Close()
			a, ok := c.LocalAddr().(*net.UDPAddr)
			if !ok {
				return nil, fmt.Errorf("unexpected local address type %T", c.LocalAddr())
			}
			return a.IP, nil
		},
		Interfaces: func() ([]Iface, error) {
			sys, err := net.Interfaces()
			if err != nil {
				return nil, err
			}
			out := make([]Iface, 0, len(sys))
			for _, si := range sys {
				ifc := Iface{Name: si.Name, Up: si.Flags&net.FlagUp != 0}
				addrs, _ := si.Addrs()
				for _, a := range addrs {
					switch v := a.(type) {
					case *net.IPNet:
						ifc.Addrs = append(ifc.Addrs, v.IP)
					case *net.IPAddr:
						ifc.Addrs = append(ifc.Addrs, v.IP)
					}
				}
				out = append(out, ifc)
			}
			return out, nil
		},
	}
}

// FromRoute returns the address on the route toward peer (when peer is a
// remote host, e.g. running off-device during development) or else on the
// default route. ok is false when no usable route exists yet.
func (f Finder) FromRoute(peer string) (ip net.IP, ok bool) {
	targets := []string{"8.8.8.8:80"}
	if p := net.ParseIP(peer); peer != "" && peer != "localhost" && (p == nil || !p.IsLoopback()) {
		targets = append([]string{net.JoinHostPort(peer, "80")}, targets...)
	}
	ifs, _ := f.Interfaces()
	for _, t := range targets {
		src, err := f.RouteSource(t)
		if err != nil || src == nil {
			continue
		}
		if Usable(ifaceOf(ifs, src), src) {
			return src.To4(), true
		}
	}
	return nil, false
}

// Fallback picks the first usable address by enumerating interfaces. It is
// only for when no route has appeared after waiting: unlike FromRoute it
// cannot tell the LAN from an internal link except via Usable's exclusions.
func (f Finder) Fallback() (net.IP, bool) {
	ifs, err := f.Interfaces()
	if err != nil {
		return nil, false
	}
	for _, ifc := range ifs {
		if !ifc.Up {
			continue
		}
		for _, ip := range ifc.Addrs {
			if Usable(ifc.Name, ip) {
				return ip.To4(), true
			}
		}
	}
	return nil, false
}

// How a Wait result was obtained.
const (
	ViaRoute    = "route"
	ViaFallback = "fallback"
	ViaLoopback = "loopback"
)

// Wait polls FromRoute every `every` until it succeeds or timeout elapses, then
// settles for Fallback, and finally 127.0.0.1. The caller should keep watching
// with FromRoute afterwards whenever the result is not ViaRoute.
func (f Finder) Wait(peer string, timeout, every time.Duration) (net.IP, string) {
	deadline := time.Now().Add(timeout)
	for {
		if ip, ok := f.FromRoute(peer); ok {
			return ip, ViaRoute
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(every)
	}
	if ip, ok := f.Fallback(); ok {
		return ip, ViaFallback
	}
	return net.IPv4(127, 0, 0, 1), ViaLoopback
}

func ifaceOf(ifs []Iface, ip net.IP) string {
	for _, ifc := range ifs {
		for _, a := range ifc.Addrs {
			if a.Equal(ip) {
				return ifc.Name
			}
		}
	}
	return ""
}
