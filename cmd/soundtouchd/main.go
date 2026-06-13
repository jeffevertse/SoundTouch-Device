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

	deviceHost := firstNonEmpty(*hostFlag, cfg.DeviceHost, "127.0.0.1")
	streamHost := localIP(deviceHost) // address the renderer can fetch our proxy from
	streamBase := fmt.Sprintf("http://%s:%d", streamHost, cfg.ProxyPort)
	log.Printf("[soundtouchd] %s | device=%s proxy=%s", version, deviceHost, streamBase)

	st := client.NewClientFromHost(deviceHost)

	// Resolve the UPnP AVTransport control URL (retry — the device may be booting).
	var player *upnp.Player
	go func() {
		for {
			if url, err := upnp.FindControlURL(deviceHost); err == nil {
				player = upnp.New(url)
				log.Printf("[soundtouchd] AVTransport control URL: %s", url)
				return
			}
			time.Sleep(10 * time.Second)
		}
	}()

	var playMu sync.Mutex
	playPreset := func(id int) error {
		p := cfg.ByID(id)
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
		cfg.LastPresetID = id
		_ = cfg.Save(*configPath)
		log.Printf("[soundtouchd] playing preset %d (%s)", id, p.Name)
		return nil
	}

	// HTTP: stream proxy + minimal control (no web UI).
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		id, ok := idFromPath(r.URL.Path, "/stream/")
		p := cfg.ByID(id)
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

	// Auto-resume on power-on.
	go func() {
		w := resume.New(st, func() {
			if cfg.LastPresetID > 0 {
				if err := playPreset(cfg.LastPresetID); err != nil {
					log.Printf("[resume] %v", err)
				}
			}
		})
		if err := w.Start(); err != nil {
			log.Printf("[resume] websocket: %v", err)
		}
	}()

	addr := fmt.Sprintf(":%d", cfg.ProxyPort)
	log.Printf("[soundtouchd] listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
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
