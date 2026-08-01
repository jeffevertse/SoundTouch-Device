package streamproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	blocked := []string{
		"127.0.0.1", "10.1.2.3", "192.168.1.5", "172.16.0.1", "169.254.169.254", "0.0.0.0", "::1",
		"100.64.0.1",                // CGNAT
		"192.0.0.170", "198.18.0.5", // IETF protocol assignments + benchmarking
		"224.0.0.251", "239.255.255.250", "255.255.255.255", "240.0.0.1", // multicast + reserved + broadcast
		"ff02::1", // IPv6 multicast
	}
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

func TestResolveLocation(t *testing.T) {
	cases := []struct{ base, loc, want string }{
		{"http://x.com/live", "https://cdn.x.com/stream", "http://cdn.x.com/stream"}, // absolute + downgrade
		{"http://x.com/a/b.pls", "/stream.mp3", "http://x.com/stream.mp3"},           // host-relative
		{"http://x.com/a/b.pls", "c.mp3", "http://x.com/a/c.mp3"},                    // path-relative
	}
	for _, c := range cases {
		if got := resolveLocation(c.base, c.loc); got != c.want {
			t.Errorf("resolveLocation(%q, %q) = %q, want %q", c.base, c.loc, got, c.want)
		}
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

// An upstream error page must not reach the renderer as if it were audio —
// otherwise the speaker plays a burst of HTML-as-noise instead of failing.
func TestRelayRejectsUpstreamError(t *testing.T) {
	w := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html>gone</html>")),
	}
	relay(w, resp, func() {})

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "<html>") {
		t.Error("upstream error body should not be relayed as audio")
	}
}

func TestRelayPassesThroughAudio(t *testing.T) {
	w := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"audio/aacp"}},
		Body:       io.NopCloser(strings.NewReader("AUDIODATA")),
	}
	relay(w, resp, func() {})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "AUDIODATA" {
		t.Errorf("body = %q, want %q", got, "AUDIODATA")
	}
	if got := w.Header().Get("Content-Type"); got != "audio/aacp" {
		t.Errorf("Content-Type = %q, want audio/aacp", got)
	}
}

// stalledBody mimics a dead upstream: the TCP connection is alive but no bytes
// arrive. It unblocks only when the request context is cancelled, which is what
// the real transport does.
type stalledBody struct{ ctx context.Context }

func (b stalledBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b stalledBody) Close() error { return nil }

// A stream that goes silent must be torn down, not left holding the connection
// (and one of the caller's concurrency slots) until TCP keepalive gives up.
func TestRelayCancelsStalledStream(t *testing.T) {
	orig := idleTimeout
	idleTimeout = 50 * time.Millisecond
	defer func() { idleTimeout = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"audio/mpeg"}},
		Body:       stalledBody{ctx: ctx},
	}

	done := make(chan struct{})
	go func() {
		relay(httptest.NewRecorder(), resp, cancel)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not return: a stalled stream was never cancelled")
	}
	if ctx.Err() == nil {
		t.Error("stall watchdog should have cancelled the request context")
	}
}

// Data flowing keeps the stream alive well past a single idleTimeout window.
func TestRelayKeepsAliveWhileDataFlows(t *testing.T) {
	orig := idleTimeout
	idleTimeout = 100 * time.Millisecond
	defer func() { idleTimeout = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	go func() {
		// 5 chunks over ~250ms, each well inside the 100ms idle window.
		for i := 0; i < 5; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := pw.Write([]byte("chunk")); err != nil {
				return
			}
		}
		pw.Close()
	}()

	w := httptest.NewRecorder()
	relay(w, &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{},
		Body:       pr,
	}, cancel)

	if ctx.Err() != nil {
		t.Error("watchdog fired despite data flowing steadily")
	}
	if got := w.Body.String(); got != strings.Repeat("chunk", 5) {
		t.Errorf("body = %q, want 5 chunks", got)
	}
}
