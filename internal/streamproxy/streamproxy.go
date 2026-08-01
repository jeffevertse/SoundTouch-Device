// Package streamproxy fetches internet-radio streams and serves them to the
// SoundTouch's own renderer over plain HTTP.
//
// Two things the SoundTouch 20 firmware needs that this handles:
//   - HTTPS is downgraded to HTTP (the speaker can't do TLS on media streams).
//   - PLS/M3U playlists are resolved to a direct stream URL.
//
// Outbound fetches are hardened against SSRF / DNS-rebinding: the host is
// resolved once, rejected if it points at a private/loopback address, and the
// connection is pinned to that exact IP with the original Host header.
package streamproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// idleTimeout bounds how long the proxy waits for the next byte of audio. A
// stalled upstream (TCP connection alive, no data flowing) would otherwise hold
// the request open until TCP keepalive gives up — minutes, during which it also
// holds one of the caller's concurrency slots. Radio streams deliver
// continuously, so a gap this long always means the stream is dead.
//
// A var, not a const, so tests can shorten it.
var idleTimeout = 20 * time.Second

// privateNets are ranges a public stream URL must never resolve to.
var privateNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"127.0.0.0/8", "169.254.0.0/16", // link-local + cloud metadata
		"100.64.0.0/10",                 // CGNAT
		"192.0.0.0/24", "198.18.0.0/15", // IETF protocol assignments + benchmarking
		"224.0.0.0/4", "240.0.0.0/4", // multicast + reserved
		"::1/128", "fc00::/7", "fe80::/10",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}()

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Normalise IPv4 (incl. IPv4-mapped IPv6) to 4-byte so it's checked against
	// the IPv4 ranges, not accidentally matched by an IPv6 net.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// lookupIP is indirected so tests can stub DNS.
var lookupIP = net.LookupIP

// downgrade rewrites an https:// URL to http://.
func downgrade(raw string) string {
	if strings.HasPrefix(raw, "https://") {
		return "http://" + raw[len("https://"):]
	}
	return raw
}

// resolvePublicIP resolves host and returns the first public IP, or an error if
// it cannot be resolved or any resolved address is private/loopback (which would
// indicate an SSRF attempt or a rebinding host).
func resolvePublicIP(host string) (string, error) {
	ips, err := lookupIP(host)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	var public string
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return "", fmt.Errorf("%q resolves to a private/loopback address (%s)", host, ip)
		}
		if public == "" {
			public = ip.String()
		}
	}
	if public == "" {
		return "", fmt.Errorf("%q did not resolve to any address", host)
	}
	return public, nil
}

// parsePlaylist extracts the first direct stream URL from PLS or M3U content,
// downgrading it to HTTP. Returns "" if nothing usable is found.
func parsePlaylist(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		// PLS: FileN=<url>
		if strings.HasPrefix(strings.ToLower(line), "file") && strings.Contains(line, "=") {
			cand := strings.TrimSpace(line[strings.Index(line, "=")+1:])
			if strings.HasPrefix(cand, "http") {
				return downgrade(cand)
			}
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		// M3U: first non-comment URL
		if line != "" && !strings.HasPrefix(line, "#") && strings.HasPrefix(line, "http") {
			return downgrade(line)
		}
	}
	return ""
}

func looksLikePlaylistExt(u string) bool {
	l := strings.ToLower(u)
	for _, ext := range []string{".pls", ".m3u", ".m3u8", ".xspf"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

func looksLikePlaylistCT(ct string) bool {
	for _, x := range []string{"scpls", "mpegurl", "xspf"} {
		if strings.Contains(ct, x) {
			return true
		}
	}
	return false
}

// safeGet performs a DNS-rebinding-safe HTTP request: downgrade to HTTP, resolve
// the host once, reject private targets, and connect to that pinned IP while
// preserving the original Host header. Redirects are not followed.
func safeGet(ctx context.Context, method, raw string, timeout time.Duration) (*http.Response, error) {
	raw = downgrade(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("only http/https URLs are allowed (got %q)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL has no hostname")
	}
	ip, err := resolvePublicIP(host)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	pinned := *u
	pinned.Host = net.JoinHostPort(ip, port)

	req, err := http.NewRequestWithContext(ctx, method, pinned.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Host = u.Host // original host[:port] for virtual-host routing
	req.Header.Set("User-Agent", "SoundTouch/1.0")
	// Deliberately do NOT request ICY metadata: the SoundTouch renderer fetches
	// our stream without asking for metadata, so any interleaved metadata blocks
	// would corrupt the audio (scrambled / fast-forward sound).
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't auto-follow; caller re-validates
		},
	}
	return client.Do(req)
}

// maxRedirects bounds how many redirect hops the proxy will chase; every hop
// is re-validated (resolve + private-IP check) before it is fetched.
const maxRedirects = 5

// resolveLocation resolves a redirect Location (possibly relative) against the
// URL that issued it, downgraded to HTTP.
func resolveLocation(base, loc string) string {
	b, err := url.Parse(base)
	if err != nil {
		return downgrade(loc)
	}
	l, err := url.Parse(loc)
	if err != nil {
		return downgrade(loc)
	}
	return downgrade(b.ResolveReference(l).String())
}

// follow performs safeGet and chases up to maxRedirects redirect hops.
// It returns the final response and the URL it came from.
func follow(ctx context.Context, method, raw string, timeout time.Duration) (*http.Response, string, error) {
	for hop := 0; ; hop++ {
		resp, err := safeGet(ctx, method, raw, timeout)
		if err != nil {
			return nil, "", err
		}
		loc := resp.Header.Get("Location")
		if !isRedirect(resp.StatusCode) || loc == "" || hop >= maxRedirects {
			return resp, raw, nil
		}
		resp.Body.Close()
		raw = resolveLocation(raw, loc)
	}
}

// Resolve turns a configured station URL into a direct HTTP stream URL,
// downgrading HTTPS and resolving PLS/M3U playlists.
func Resolve(ctx context.Context, raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty URL")
	}
	raw = downgrade(raw)
	isPlaylist := looksLikePlaylistExt(raw)
	if !isPlaylist {
		if head, final, err := follow(ctx, http.MethodHead, raw, 5*time.Second); err == nil {
			ct := head.Header.Get("Content-Type")
			head.Body.Close()
			raw = final
			isPlaylist = looksLikePlaylistCT(ct) || looksLikePlaylistExt(final)
		}
	}
	if !isPlaylist {
		return raw, nil
	}
	resp, _, err := follow(ctx, http.MethodGet, raw, 10*time.Second)
	if err != nil {
		return raw, nil // fall back to original on fetch failure
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if direct := parsePlaylist(body); direct != "" {
		return direct, nil
	}
	return raw, nil
}

func isRedirect(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// openStream opens a long-lived HTTP GET for audio streaming. Unlike safeGet,
// the http.Client has no overall Timeout so the body can be read indefinitely;
// connection and header deadlines are enforced at the transport level, and ctx
// tears the connection down when the client goes away or the stream stalls.
func openStream(ctx context.Context, raw string) (*http.Response, error) {
	raw = downgrade(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("only http/https URLs are allowed (got %q)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("URL has no hostname")
	}
	ip, err := resolvePublicIP(host)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	pinned := *u
	pinned.Host = net.JoinHostPort(ip, port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pinned.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Host = u.Host
	req.Header.Set("User-Agent", "SoundTouch/1.0")
	c := &http.Client{
		Timeout: 0, // no overall deadline — stream runs as long as data flows
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c.Do(req)
}

// Proxy streams the resolved station to w as clean audio (no ICY metadata).
// The upstream fetch is tied to r's context, so it is torn down as soon as the
// renderer disconnects rather than lingering on a socket nobody is reading.
func Proxy(w http.ResponseWriter, r *http.Request, stationURL string) {
	// Derived from the request context so both a client disconnect and the
	// stall watchdog below abort the upstream read.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	direct, err := Resolve(ctx, stationURL)
	if err != nil {
		http.Error(w, "no stream", http.StatusNotFound)
		return
	}
	resp, err := openStream(ctx, direct)
	if err != nil {
		http.Error(w, "stream error", http.StatusBadGateway)
		return
	}
	for hop := 0; isRedirect(resp.StatusCode) && hop < maxRedirects; hop++ {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			http.Error(w, "stream error", http.StatusBadGateway)
			return
		}
		direct = resolveLocation(direct, loc)
		resp, err = openStream(ctx, direct)
		if err != nil {
			http.Error(w, "stream error", http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	relay(w, resp, cancel)
}

// relay validates an upstream response and copies its body to w as clean audio,
// calling cancel if the stream falls silent for idleTimeout. Split out from
// Proxy so it can be tested without a live upstream.
func relay(w http.ResponseWriter, resp *http.Response, cancel context.CancelFunc) {
	// Don't hand the renderer an error page as if it were audio. This also
	// catches a redirect chain that outran maxRedirects.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		http.Error(w, "upstream returned "+resp.Status, http.StatusBadGateway)
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "audio/mpeg"
	}
	w.Header().Set("Content-Type", ct)
	// Intentionally do not forward icy-* headers: we don't request metadata, so
	// the body is clean audio and the renderer must not expect interleaved metadata.

	// Cancel the fetch if the stream goes quiet for idleTimeout; every chunk of
	// audio pushes the deadline out again. Cancelling aborts the in-flight
	// Read below, which is what frees the connection and the caller's slot.
	stall := time.AfterFunc(idleTimeout, cancel)
	defer stall.Stop()

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(idleTimeout)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
