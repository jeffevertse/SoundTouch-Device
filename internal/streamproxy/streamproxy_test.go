package streamproxy

import (
	"fmt"
	"net"
	"testing"
)

func TestDowngrade(t *testing.T) {
	cases := map[string]string{
		"https://x.com/a.mp3":      "http://x.com/a.mp3",
		"https://x.com:8443/live":  "http://x.com:8443/live",
		"http://x.com/a.mp3":       "http://x.com/a.mp3",
		"http://x.com/already.mp3": "http://x.com/already.mp3",
	}
	for in, want := range cases {
		if got := downgrade(in); got != want {
			t.Errorf("downgrade(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "192.168.1.5", "172.16.0.1", "169.254.169.254", "0.0.0.0", "::1"}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "1.1.1.1"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestResolvePublicIP(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()

	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	if ip, err := resolvePublicIP("example.com"); err != nil || ip != "93.184.216.34" {
		t.Fatalf("public: got %q,%v", ip, err)
	}

	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("192.168.1.9")}, nil }
	if _, err := resolvePublicIP("evil.test"); err == nil {
		t.Fatal("private address should be rejected")
	}

	// Mixed public+private must be rejected (anti DNS-rebinding).
	lookupIP = func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.5")}, nil
	}
	if _, err := resolvePublicIP("rebind.test"); err == nil {
		t.Fatal("mixed public/private should be rejected")
	}

	lookupIP = func(string) ([]net.IP, error) { return nil, fmt.Errorf("nxdomain") }
	if _, err := resolvePublicIP("nope.test"); err == nil {
		t.Fatal("unresolvable should error")
	}
}

func TestParsePlaylist(t *testing.T) {
	pls := []byte("[playlist]\nNumberOfEntries=2\nFile1=https://cdn.example.com/stream\nFile2=http://x/2\n")
	if got := parsePlaylist(pls); got != "http://cdn.example.com/stream" {
		t.Errorf("PLS: got %q", got)
	}
	m3u := []byte("#EXTM3U\n#EXTINF:-1,Radio\nhttps://secure.example.com/live\n")
	if got := parsePlaylist(m3u); got != "http://secure.example.com/live" {
		t.Errorf("M3U: got %q", got)
	}
	if got := parsePlaylist([]byte("not a playlist")); got != "" {
		t.Errorf("non-playlist should be empty, got %q", got)
	}
}

func TestLooksLikePlaylist(t *testing.T) {
	if !looksLikePlaylistExt("http://x/a.pls") || !looksLikePlaylistExt("http://x/a.m3u8") {
		t.Error("ext detection failed")
	}
	if looksLikePlaylistExt("http://x/a.mp3") {
		t.Error("mp3 is not a playlist")
	}
	if !looksLikePlaylistCT("audio/x-scpls") || !looksLikePlaylistCT("application/vnd.apple.mpegurl") {
		t.Error("content-type detection failed")
	}
}
