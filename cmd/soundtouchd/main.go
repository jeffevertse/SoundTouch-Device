// Command soundtouchd runs on the Bose SoundTouch itself: it serves internet-radio
// streams to the speaker's own renderer (HTTPS→HTTP, playlist resolution), plays
// presets via UPnP AVTransport, and auto-resumes the last station on power-on.
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gesellix/bose-soundtouch/pkg/client"
	"github.com/gesellix/bose-soundtouch/pkg/models"

	"github.com/jeffevertse/soundtouch-device/internal/presets"
	"github.com/jeffevertse/soundtouch-device/internal/resume"
	"github.com/jeffevertse/soundtouch-device/internal/streamproxy"
	"github.com/jeffevertse/soundtouch-device/internal/upnp"
)

var version = "dev"

// The config editor is served from the daemon itself (GET /) so it is
// same-origin with the API — no CORS or file:// special-casing needed.
//
//go:embed editor.html
var editorHTML []byte

// configStore is the thread-safe holder for the live config. Replace swaps the
// whole pointer (hot-reload); readers get a consistent snapshot via Get.
type configStore struct {
	mu   sync.RWMutex
	cfg  *presets.Config
	path string
}

func (s *configStore) Get() *presets.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *configStore) LastPreset() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.LastPresetID
}

func (s *configStore) SetLastPreset(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.LastPresetID == id {
		return // avoid needless flash writes on /mnt/nv
	}
	// Copy-on-write: snapshots handed out by Get() are never mutated.
	c := s.cfg.Clone()
	c.LastPresetID = id
	_ = c.Save(s.path)
	s.cfg = c
}

// Replace validates, persists, and swaps in a new config.
func (s *configStore) Replace(c *presets.Config) error {
	if err := presets.Validate(c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := c.Save(s.path); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

// requireMutation gates state-changing requests: the JSON Content-Type blocks
// cross-site form posts (forms can't send application/json), and when an
// api_token is configured the request must carry it as a Bearer token.
func requireMutation(w http.ResponseWriter, r *http.Request, cfg *presets.Config) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	if cfg.APIToken != "" {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(cfg.APIToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
	}
	return true
}

func main() {
	configPath := flag.String("config", "/mnt/nv/soundtouchd/config.json", "path to config.json")
	hostFlag := flag.String("host", "", "SoundTouch host (default: 127.0.0.1, i.e. this device)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := presets.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	store := &configStore{cfg: cfg, path: *configPath}

	deviceHost := firstNonEmpty(*hostFlag, cfg.DeviceHost, "127.0.0.1")
	listenPort := cfg.ProxyPort // fixed at startup; changing it needs a restart
	log.Printf("[soundtouchd] %s | device=%s port=%d", version, deviceHost, listenPort)

	st := client.NewClientFromHost(deviceHost)

	// Wait for the SoundTouch firmware HTTP API (port 8090) to be ready.
	// soundtouchd starts before the firmware on cold boot; this is the single
	// gate for everything that depends on the API. Network (DHCP) is also
	// guaranteed to be up once GetDeviceInfo() succeeds over TCP.
	deviceID := ""
	for {
		if info, err := st.GetDeviceInfo(); err == nil {
			deviceID = info.DeviceID
			log.Printf("[soundtouchd] firmware ready: %s (%s)", info.Name, deviceID)
			break
		}
		log.Printf("[soundtouchd] waiting for firmware API...")
		time.Sleep(5 * time.Second)
	}

	// Compute streamHost after the firmware gate: the network (DHCP) is now
	// guaranteed up, so localIP returns the real LAN IP rather than 127.0.0.1.
	streamHost := localIP(deviceHost)
	streamBase := fmt.Sprintf("http://%s:%d", streamHost, listenPort)
	log.Printf("[soundtouchd] proxy=%s", streamBase)

	// Resolve the UPnP AVTransport control URL. Retry in case the renderer
	// comes up slightly after the HTTP API. atomic.Pointer: written here,
	// read by HTTP handlers on other goroutines.
	var player atomic.Pointer[upnp.Player]
	go func() {
		for {
			if url, err := upnp.FindControlURL(streamHost, deviceID); err == nil {
				player.Store(upnp.New(url))
				log.Printf("[soundtouchd] AVTransport control URL: %s", url)
				return
			}
			time.Sleep(5 * time.Second)
		}
	}()

	// Point the physical preset buttons at this daemon. Retry with backoff for
	// transient errors right after boot, but give up eventually: a permanently
	// rejected preset would otherwise hammer the device API and grow the log
	// forever, on a device with a very small writable partition.
	go retryPresetSync(st, store.Get, streamBase)

	var playMu sync.Mutex
	playPreset := func(id int) error {
		p := store.Get().ByID(id)
		if p == nil || p.StreamURL == "" {
			return fmt.Errorf("preset %d not configured", id)
		}
		pl := player.Load()
		if pl == nil {
			return fmt.Errorf("renderer not ready yet")
		}
		playMu.Lock()
		defer playMu.Unlock()
		streamURL := fmt.Sprintf("%s/stream/%d", streamBase, id)
		if err := pl.Play(streamURL); err != nil {
			return err
		}
		store.SetLastPreset(id)
		log.Printf("[soundtouchd] playing preset %d (%s)", id, p.Name)
		return nil
	}

	mux := http.NewServeMux()

	// Config editor UI (same-origin with the API, so no CORS involved).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(editorHTML)
	})

	// Cap concurrent outbound stream fetches — the speaker itself needs 1;
	// this keeps a misbehaving LAN client from exhausting the device's RAM.
	streamSem := make(chan struct{}, 4)
	mux.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		// Same guard as /play, here to stop a drive-by page from parking <audio>
		// elements on us and eating the slots below. The speaker's own renderer
		// is not a browser and sends no Fetch-Metadata headers.
		if !sameSiteOK(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		id, ok := idFromPath(r.URL.Path, "/stream/")
		p := store.Get().ByID(id)
		if !ok || p == nil || p.StreamURL == "" {
			http.NotFound(w, r)
			return
		}
		select {
		case streamSem <- struct{}{}:
			defer func() { <-streamSem }()
		default:
			http.Error(w, "too many concurrent streams", http.StatusServiceUnavailable)
			return
		}
		streamproxy.Proxy(w, r, p.StreamURL)
	})

	mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
		// /play is a GET (the iOS app and curl rely on that), so it can't use the
		// JSON Content-Type guard the POST endpoints do. Fetch-Metadata is the
		// equivalent for a GET: it stops a public page from starting playback with
		// an <img src=…/play/1>, while non-browser clients send no such headers
		// and are unaffected.
		if !sameSiteOK(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		id, ok := idFromPath(r.URL.Path, "/play/")
		if !ok {
			http.Error(w, "bad preset id", http.StatusBadRequest)
			return
		}
		if err := playPreset(id); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "preset": id})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": version, "rendererReady": player.Load() != nil})
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		np, err := st.GetNowPlaying()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, np)
	})

	// ── config editor API (used by editor/config-editor.html) ───────────────
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Never expose the token; shallow copy is safe (scalar field only).
			c := *store.Get()
			c.APIToken = ""
			writeJSON(w, &c)
		case http.MethodPost, http.MethodPut:
			if !requireMutation(w, r, store.Get()) {
				return
			}
			var c presets.Config
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if c.APIToken == "" {
				// GET /config redacts the token, so round-tripped configs come
				// back without it — keep the existing one rather than wiping it.
				c.APIToken = store.Get().APIToken
			}
			if err := store.Replace(&c); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			log.Printf("[soundtouchd] config updated via API (%d presets)", len(c.Presets))
			// An edited stream_url must take effect now, not when the proxy's
			// resolution cache happens to expire.
			streamproxy.InvalidateCache()
			go syncHardwarePresets(st, &c, streamBase)
			writeJSON(w, map[string]any{"ok": true, "restartNeeded": c.ProxyPort != listenPort})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/bass", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			b, err := st.GetBass()
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, map[string]any{"level": b.ActualBass})
		case http.MethodPost, http.MethodPut:
			if !requireMutation(w, r, store.Get()) {
				return
			}
			var req struct {
				Level int `json:"level"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := st.SetBassSafe(req.Level); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			// SetBassSafe clamps to the device's range; report what actually
			// landed, not what was asked for, so the editor's slider can't drift
			// out of sync with the speaker.
			level := models.ClampBassLevel(req.Level)
			log.Printf("[soundtouchd] bass set to %d", level)
			writeJSON(w, map[string]any{"ok": true, "level": level})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !requireMutation(w, r, store.Get()) {
			return
		}
		writeJSON(w, map[string]any{"ok": true, "restarting": true})
		go func() {
			time.Sleep(500 * time.Millisecond)
			log.Printf("[soundtouchd] restart requested via API")
			_ = exec.Command("/etc/init.d/soundtouchd", "restart").Run()
		}()
	})

	// Auto-resume on power-on + physical preset buttons. Retries on failure so
	// a cold boot (firmware WebSocket not ready yet) self-heals.
	go func() {
		watcher := resume.New(st, func() {
			// Never wake a speaker that is actually off (guards a late/stale event).
			if np, err := st.GetNowPlaying(); err == nil && strings.EqualFold(np.Source, "STANDBY") {
				log.Printf("[resume] device is in standby — not waking it")
				return
			}
			if id := store.LastPreset(); id > 0 {
				if err := playPreset(id); err != nil {
					log.Printf("[resume] %v", err)
				}
			}
		}, func(id int) {
			if err := playPreset(id); err != nil {
				log.Printf("[preset] %v", err)
			}
		})
		for {
			// Start blocks while connected; the library reconnects internally on
			// drop. It only returns an error when the initial connect fails (firmware
			// WebSocket not up yet at cold boot). Retry until the firmware is ready.
			if err := watcher.Start(); err != nil {
				log.Printf("[resume] websocket error: %v — retrying in 5s", err)
				time.Sleep(5 * time.Second)
			} else {
				return // context cancelled / explicit disconnect — not expected in normal use
			}
		}
	}()

	// HTTP server: mux is fully set up by this point. WriteTimeout must stay 0
	// (unset) — /stream responses are unbounded audio.
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", listenPort),
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second, // Slowloris guard
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("[soundtouchd] listening on %s", srv.Addr)
		log.Fatal(srv.ListenAndServe())
	}()

	select {} // goroutines run forever; this keeps main alive
}

// Bounds on the preset-sync retry loop: ~8 minutes of attempts, which comfortably
// covers a cold boot, then stop rather than retrying forever.
const (
	maxPresetSyncAttempts = 12
	maxPresetSyncDelay    = 60 * time.Second
)

// retryPresetSync calls syncHardwarePresets until it succeeds, backing off from
// 5s to maxPresetSyncDelay and giving up after maxPresetSyncAttempts. The next
// config change triggers a fresh sync regardless.
//
// current is re-read every attempt rather than captured once, so a config saved
// while we're still retrying is the one that lands in the hardware slots.
func retryPresetSync(st *client.Client, current func() *presets.Config, streamBase string) {
	delay := 5 * time.Second
	for attempt := 1; ; attempt++ {
		if syncHardwarePresets(st, current(), streamBase) {
			return
		}
		if attempt >= maxPresetSyncAttempts {
			log.Printf("[soundtouchd] hardware preset sync still failing after %d attempts — giving up "+
				"(presets still work via /play and the config editor; save the config again to retry)", attempt)
			return
		}
		time.Sleep(delay)
		delay = min(2*delay, maxPresetSyncDelay)
	}
}

// syncHardwarePresets writes the configured stations into the speaker's 6 physical
// preset slots as LOCAL_INTERNET_RADIO entries pointing at this daemon's stream
// proxy, so pressing a physical button (or app preset) plays via us — not the dead
// Bose cloud. Re-run whenever the config changes. Returns true if all StorePreset
// calls succeeded (callers may retry on false).
func syncHardwarePresets(st *client.Client, cfg *presets.Config, streamBase string) bool {
	ok := true
	for _, p := range cfg.Presets {
		if p.StreamURL == "" {
			continue
		}
		ci := &models.ContentItem{
			Source:       "LOCAL_INTERNET_RADIO",
			Type:         "stationurl",
			Location:     fmt.Sprintf("%s/stream/%d", streamBase, p.ID),
			IsPresetable: true,
			ItemName:     p.Name,
		}
		if err := st.StorePreset(p.ID, ci); err != nil {
			log.Printf("[soundtouchd] storePreset %d: %v", p.ID, err)
			ok = false
		}
	}
	if ok {
		log.Printf("[soundtouchd] hardware presets synced -> %s/stream/<id>", streamBase)
	}
	return ok
}

// cors allows pages served from localhost/the LAN (e.g. a Home Assistant
// dashboard) to call the API, while public websites get no CORS header and are
// blocked by the browser. Origin "null" (file:// pages, but also sandboxed
// iframes on public sites) is deliberately NOT allowed — the bundled editor is
// served same-origin from GET / instead.
func cors(w http.ResponseWriter, r *http.Request) {
	o := r.Header.Get("Origin")
	if isLocalOrigin(o) {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	}
}

// withCORS applies cors to every endpoint and answers preflight uniformly.
// Doing this once in middleware rather than per handler is what keeps /status
// and /healthz reachable from a LAN dashboard — they were previously left out.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cors(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameSiteOK reports whether a request may perform a side effect via GET.
// Browsers label every request with Sec-Fetch-Site; "cross-site" means some
// other page triggered it, which for /play or /stream is never legitimate.
// Non-browser clients (curl, the iOS app, the speaker's own renderer) send
// neither header and are always allowed.
func sameSiteOK(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "same-site", "none":
		// "none" is a user-initiated load: address bar, bookmark, app.
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && !isLocalOrigin(o) {
		return false
	}
	return true
}

func isLocalOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func idFromPath(path, prefix string) (int, bool) {
	id, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(path, prefix), "/"))
	if err != nil {
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// localIP returns this machine's outbound LAN IP (the address the renderer can
// use to reach our stream proxy). Falls back to interface enumeration (works
// when the external routing table is not yet populated at cold boot).
func localIP(peer string) string {
	for _, target := range []string{net.JoinHostPort(peer, "80"), "8.8.8.8:80"} {
		conn, err := net.DialTimeout("udp", target, 2*time.Second)
		if err != nil {
			continue
		}
		addr, ok := conn.LocalAddr().(*net.UDPAddr)
		conn.Close()
		if !ok {
			continue // never expected for a udp dial; don't panic the daemon at boot
		}
		if addr.IP != nil && !addr.IP.IsLoopback() {
			return addr.IP.String()
		}
	}
	// No external route yet (early boot) — enumerate interfaces directly.
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip4 := ip.To4(); ip4 != nil {
					return ip4.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
