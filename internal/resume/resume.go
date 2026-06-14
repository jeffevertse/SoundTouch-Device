// Package resume watches the SoundTouch WebSocket and (1) re-plays the last
// station when the speaker powers on, and (2) plays the matching preset via UPnP
// when a physical preset button is pressed — the speaker's native recall of
// LOCAL_INTERNET_RADIO presets is unreliable, so we drive playback ourselves.
package resume

import (
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gesellix/bose-soundtouch/pkg/client"
	"github.com/gesellix/bose-soundtouch/pkg/models"
)

const (
	resumeDebounce = 10 * time.Second // ignore repeat power-on triggers
	pressDebounce  = 3 * time.Second  // ignore repeat button-press frames
)

// matches the selected preset id inside a nowSelectionUpdated frame
var presetIDRe = regexp.MustCompile(`nowSelectionUpdated[\s\S]*?<preset[^>]*\bid="(\d+)"`)

// Watcher reacts to power-on (onResume) and physical preset presses (onPreset).
type Watcher struct {
	client     *client.Client
	onResume   func()
	onPreset   func(int)
	mu         sync.Mutex
	prev       string
	lastResume time.Time
	lastPress  time.Time
}

func New(c *client.Client, onResume func(), onPreset func(int)) *Watcher {
	return &Watcher{client: c, onResume: onResume, onPreset: onPreset}
}

func (w *Watcher) Start() error {
	ws := w.client.NewWebSocketClient(client.DefaultWebSocketConfig())

	// Power-on → resume last station.
	ws.OnNowPlaying(func(ev *models.NowPlayingUpdatedEvent) {
		src := strings.ToUpper(strings.TrimSpace(ev.NowPlaying.Source))
		w.mu.Lock()
		prev := w.prev
		w.prev = src
		powerOn := prev == "STANDBY" && src != "" && src != "STANDBY"
		fire := powerOn && time.Since(w.lastResume) >= resumeDebounce
		if fire {
			w.lastResume = time.Now()
		}
		w.mu.Unlock()
		switch {
		case fire:
			log.Printf("[resume] power-on detected (%s -> %s) — resuming", prev, src)
			go w.onResume()
		case powerOn:
			log.Printf("[resume] power-on (%s -> %s) ignored (debounced)", prev, src)
		}
	})

	// Physical preset button → play that preset via UPnP.
	ws.OnRawMessage(func(data []byte, _ error) {
		s := string(data)
		if !strings.Contains(s, "nowSelectionUpdated") {
			return
		}
		m := presetIDRe.FindStringSubmatch(s)
		if m == nil {
			return
		}
		id, err := strconv.Atoi(m[1])
		if err != nil || id < 1 {
			return
		}
		w.mu.Lock()
		recent := time.Since(w.lastPress) < pressDebounce
		if !recent {
			w.lastPress = time.Now()
		}
		w.mu.Unlock()
		if recent {
			return
		}
		log.Printf("[preset] physical button %d pressed — playing via UPnP", id)
		go func() {
			time.Sleep(2 * time.Second) // let the speaker's own (often-failing) recall settle
			w.onPreset(id)
		}()
	})

	return ws.Connect()
}
