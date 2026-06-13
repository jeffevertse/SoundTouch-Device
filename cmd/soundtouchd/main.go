// Command soundtouchd runs on the Bose SoundTouch itself: it serves internet-radio
// streams to the speaker's own renderer (HTTPS→HTTP, playlist resolution), plays
// presets via UPnP AVTransport, and auto-resumes the last station on power-on.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gesellix/bose-soundtouch/pkg/client"

	"github.com/jeffevertse/soundtouch-device/internal/presets"
	"github.com/jeffevertse/soundtouch-device/internal/resume"
	"github.com/jeffevertse/soundtouch-device/internal/streamproxy"
	"github.com/jeffevertse/soundtouch-device/internal/upnp"
)

var version = "dev"

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
	s.cfg.LastPresetID = id
	_ = s.cfg.Save(s.path)
}

// Replace validates, persists, and swaps in a new config.
func (s *configStore) Replace(c *presets.Config) error {
	if err := validateConfig(c); err != nil {
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

func validateConfig(c *presets.Config) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return fmt.Errorf("proxy_port %d out of range", c.ProxyPort)
	}
	if len(c.Presets) == 0 {
		return fmt.Errorf("no presets")
	}
	for _, p := range c.Presets {
		if p.ID < 1 || p.ID > 6 {
			return fmt.Errorf("preset id %d out of range (1-6)", p.ID)
		}
	}
	return nil
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
	streamHost := localIP(deviceHost) // address the renderer can fetch our proxy from
	listenPort := cfg.ProxyPort       // fixed at startup; changing it needs a restart
	streamBase := fmt.Sprintf("http://%s:%d", streamHost, listenPort)
	log.Printf("[soundtouchd] %s | device=%s proxy=%s", version, deviceHost, streamBase)

	st := client.NewClientFromHost(deviceHost)

	deviceID := ""
	if info, err := st.GetDeviceInfo(); err == nil {
		deviceID = info.DeviceID
		log.Printf("[soundtouchd] device: %s (%s)", info.Name, deviceID)
	}

	// Resolve the UPnP AVTransport control URL (retry — the device may be booting).
	var player *upnp.Player
	go func() {
		for {
			if url, err := upnp.FindControlURL(streamHost, deviceID); err == nil {
				player = upnp.New(url)
				log.Printf("[soundtouchd] AVTransport control URL: %s", url)
				return
			}
			time.Sleep(10 * time.Second)
		}
	}()

	var playMu sync.Mutex
	playPreset := func(id int) error {
		p := store.Get().ByID(id)
		if p == nil || p.StreamURL == "" {
			return fmt.Errorf("preset %d not configured", id)
		}
		if player == nil {
			return fmt.Errorf("renderer not ready yet")
		}
		playMu.Lock()
		defer playMu.Unlock()
		streamURL := fmt.Sprintf("%s/stream/%d", streamBase, id)
		if err := player.Play(streamURL); err != nil {
			return err
		}
		store.SetLastPreset(id)
		log.Printf("[soundtouchd] playing preset %d (%s)", id, p.Name)
		return nil
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		id, ok := idFromPath(r.URL.Path, "/stream/")
		p := store.Get().ByID(id)
		if !ok || p == nil || p.StreamURL == "" {
			http.NotFound(w, r)
			return
		}
		streamproxy.Proxy(w, p.StreamURL)
	})

	mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, map[string]any{"ok": true, "version": version, "rendererReady": player != nil})
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
		cors(w, r)
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			writeJSON(w, store.Get())
		case http.MethodPost, http.MethodPut:
			var c presets.Config
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := store.Replace(&c); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			log.Printf("[soundtouchd] config updated via API (%d presets)", len(c.Presets))
			writeJSON(w, map[string]any{"ok": true, "restartNeeded": c.ProxyPort != listenPort})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		cors(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "restarting": true})
		go func() {
			time.Sleep(500 * time.Millisecond)
			log.Printf("[soundtouchd] restart requested via API")
			_ = exec.Command("/etc/init.d/soundtouchd", "restart").Run()
		}()
	})

	// Auto-resume on power-on.
	go func() {
		watcher := resume.New(st, func() {
			if id := store.LastPreset(); id > 0 {
				if err := playPreset(id); err != nil {
					log.Printf("[resume] %v", err)
				}
			}
		})
		if err := watcher.Start(); err != nil {
			log.Printf("[resume] websocket: %v", err)
		}
	}()

	addr := fmt.Sprintf(":%d", listenPort)
	log.Printf("[soundtouchd] listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// cors allows the local config editor (a file:// page → Origin "null", or one
// served from localhost/the LAN) to call the API, while public websites get no
// CORS header and are blocked by the browser.
func cors(w http.ResponseWriter, r *http.Request) {
	o := r.Header.Get("Origin")
	if o == "null" || isLocalOrigin(o) {
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
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
// use to reach our stream proxy). Falls back to 127.0.0.1.
func localIP(peer string) string {
	for _, target := range []string{net.JoinHostPort(peer, "80"), "8.8.8.8:80"} {
		conn, err := net.DialTimeout("udp", target, 2*time.Second)
		if err != nil {
			continue
		}
		ip := conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
		if ip != nil && !ip.IsLoopback() {
			return ip.String()
		}
	}
	return "127.0.0.1"
}
