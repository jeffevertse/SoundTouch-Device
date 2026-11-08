package hostaddr

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

var errNoRoute = errors.New("connect: network is unreachable")

// coldBoot is the speaker's real interface table, captured over SSH on a cold
// boot: eth0 unused, usb0 is Bose's internal gadget link, wlan0 the LAN.
func coldBoot(wlanHasLease bool) []Iface {
	wlan := Iface{Name: "wlan0", Up: true}
	if wlanHasLease {
		wlan.Addrs = []net.IP{net.ParseIP("192.168.1.29")}
	}
	return []Iface{
		{Name: "eth0", Up: true},
		{Name: "lo", Up: true, Addrs: []net.IP{net.ParseIP("127.0.0.1")}},
		{Name: "usb0", Up: true, Addrs: []net.IP{net.ParseIP("203.0.113.1")}},
		wlan,
	}
}

func finder(route func(string) (net.IP, error), ifs []Iface) Finder {
	return Finder{
		RouteSource: route,
		Interfaces:  func() ([]Iface, error) { return ifs, nil },
	}
}

func noRoute(string) (net.IP, error) { return nil, errNoRoute }

func routeVia(ip string) func(string) (net.IP, error) {
	return func(string) (net.IP, error) { return net.ParseIP(ip), nil }
}

func TestUsable(t *testing.T) {
	cases := []struct {
		iface, ip string
		want      bool
	}{
		{"wlan0", "192.168.1.29", true},
		{"eth0", "10.0.0.5", true},
		{"usb0", "203.0.113.1", false},   // the cold-boot bug
		{"wlan0", "203.0.113.1", false},  // documentation range, whatever the interface
		{"usb0", "192.168.7.2", false},   // gadget link, whatever the address
		{"lo", "127.0.0.1", false},       // loopback
		{"wlan0", "169.254.10.1", false}, // APIPA: DHCP failed
		{"wlan0", "0.0.0.0", false},
		{"wlan0", "fe80::1", false}, // IPv6 — the renderer path is IPv4
	}
	for _, c := range cases {
		if got := Usable(c.iface, net.ParseIP(c.ip)); got != c.want {
			t.Errorf("Usable(%s, %s) = %v, want %v", c.iface, c.ip, got, c.want)
		}
	}
}

func TestFromRoute(t *testing.T) {
	// The normal case: a default route via wlan0.
	f := finder(routeVia("192.168.1.29"), coldBoot(true))
	if ip, ok := f.FromRoute("127.0.0.1"); !ok || ip.String() != "192.168.1.29" {
		t.Errorf("default route: got %v %v, want 192.168.1.29", ip, ok)
	}

	// No route yet (DHCP still pending) must report not-ok, not guess.
	f = finder(noRoute, coldBoot(false))
	if ip, ok := f.FromRoute("127.0.0.1"); ok {
		t.Errorf("no route: got %v, want not ok", ip)
	}

	// A route whose source is the gadget link is rejected too.
	f = finder(routeVia("203.0.113.1"), coldBoot(false))
	if ip, ok := f.FromRoute("127.0.0.1"); ok {
		t.Errorf("route via usb0: got %v, want not ok", ip)
	}
}

// A remote peer (running off-device in development) is routed toward first;
// a loopback peer is skipped since it would only ever yield 127.0.0.1.
func TestFromRouteTriesRemotePeerFirst(t *testing.T) {
	var targets []string
	route := func(target string) (net.IP, error) {
		targets = append(targets, target)
		return net.ParseIP("192.168.1.72"), nil
	}
	f := finder(route, []Iface{{Name: "en0", Up: true, Addrs: []net.IP{net.ParseIP("192.168.1.72")}}})

	f.FromRoute("192.168.1.29")
	if len(targets) == 0 || targets[0] != "192.168.1.29:80" {
		t.Errorf("remote peer: first target %v, want 192.168.1.29:80", targets)
	}

	targets = nil
	f.FromRoute("127.0.0.1")
	if len(targets) == 0 || targets[0] != "8.8.8.8:80" {
		t.Errorf("loopback peer: first target %v, want 8.8.8.8:80", targets)
	}
}

// Fallback against the exact cold-boot interface table: it must pick wlan0's
// lease when there is one, and refuse usb0 when there isn't.
func TestFallbackColdBoot(t *testing.T) {
	f := finder(noRoute, coldBoot(true))
	if ip, ok := f.Fallback(); !ok || ip.String() != "192.168.1.29" {
		t.Errorf("with lease: got %v %v, want 192.168.1.29", ip, ok)
	}

	f = finder(noRoute, coldBoot(false))
	if ip, ok := f.Fallback(); ok {
		t.Errorf("before lease: got %v, want nothing (never usb0)", ip)
	}
}

func TestFallbackSkipsDownInterfaces(t *testing.T) {
	ifs := []Iface{
		{Name: "eth0", Up: false, Addrs: []net.IP{net.ParseIP("10.0.0.9")}},
		{Name: "wlan0", Up: true, Addrs: []net.IP{net.ParseIP("192.168.1.29")}},
	}
	if ip, _ := finder(noRoute, ifs).Fallback(); ip.String() != "192.168.1.29" {
		t.Errorf("got %v, want the up interface's address", ip)
	}
}

func TestWait(t *testing.T) {
	// Route already there: returned on the first poll.
	ip, how := finder(routeVia("192.168.1.29"), coldBoot(true)).Wait("127.0.0.1", time.Second, time.Millisecond)
	if how != ViaRoute || ip.String() != "192.168.1.29" {
		t.Errorf("immediate route: got %v via %s", ip, how)
	}

	// Route appears after a few polls (DHCP completing): still ViaRoute.
	var calls atomic.Int32
	late := func(string) (net.IP, error) {
		if calls.Add(1) < 4 {
			return nil, errNoRoute
		}
		return net.ParseIP("192.168.1.29"), nil
	}
	ip, how = finder(late, coldBoot(true)).Wait("127.0.0.1", time.Second, time.Millisecond)
	if how != ViaRoute || ip.String() != "192.168.1.29" {
		t.Errorf("late route: got %v via %s", ip, how)
	}

	// No route ever, lease present: falls back to wlan0 — not usb0.
	ip, how = finder(noRoute, coldBoot(true)).Wait("127.0.0.1", 5*time.Millisecond, time.Millisecond)
	if how != ViaFallback || ip.String() != "192.168.1.29" {
		t.Errorf("fallback: got %v via %s", ip, how)
	}

	// Nothing usable at all: loopback, never usb0.
	ip, how = finder(noRoute, coldBoot(false)).Wait("127.0.0.1", 5*time.Millisecond, time.Millisecond)
	if how != ViaLoopback || ip.String() != "127.0.0.1" {
		t.Errorf("nothing usable: got %v via %s", ip, how)
	}
}
