// Package resume watches the SoundTouch WebSocket and (1) re-plays the last
// station when the speaker powers on, and (2) plays the matching preset via UPnP
// when a physical preset button is pressed — the speaker's native recall of
// LOCAL_INTERNET_RADIO presets is unreliable, so we drive playback ourselves.
package resume

import (
	"context"
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
	resumeDebounce  = 10 * time.Second // ignore repeat power-on triggers
	pressDebounce   = 3 * time.Second  // ignore repeat button-press frames
	minStandbyDwell = 15 * time.Second // must be in standby this long before a wake counts as a real power-on
)

// matches the selected preset id inside a nowSelectionUpdated frame
var presetIDRe = regexp.MustCompile(`nowSelectionUpdated[\s\S]*?<preset[^>]*\bid="(\d+)"`)

// Watcher reacts to power-on (onResume) and physical preset presses (onPreset).
type Watcher struct {
	client      *client.Client
	onResume    func()
	onPreset    func(int)
	mu          sync.Mutex
	prev        string
	wentStandby time.Time
	lastResume  time.Time
	lastPress   time.Time
}

func New(c *client.Client, onResume func(), onPreset func(int)) *Watcher {
	return &Watcher{client: c, onResume: onResume, onPreset: onPreset}
}

// Start connects to the WebSocket and blocks until ctx is cancelled or the
// client is disconnected. The library handles reconnects internally. Start
// returns a non-nil error when the initial connect fails (e.g. firmware
// WebSocket not ready yet at boot), or ctx.Err() after cancellation.
func (w *Watcher) Start(ctx context.Context) error {
	ws := w.client.NewWebSocketClient(client.DefaultWebSocketConfig())

	// Power-on → resume last station.
	ws.OnNowPlaying(func(ev *models.NowPlayingUpdatedEvent) {
		src := strings.ToUpper(strings.TrimSpace(ev.NowPlaying.Source))
		w.mu.Lock()
		prev := w.prev
		w.prev = src
		if src == "STANDBY" && prev != "STANDBY" {
			w.wentStandby = time.Now()
		}
		dwell := time.Since(w.wentStandby)
		powerOn := prev == "STANDBY" && src != "" && src != "STANDBY"
		// A real power-on follows a long standby; the power-OFF flicker
		// (STANDBY → transient source) has near-zero dwell and must be ignored.
		fire := powerOn && dwell >= minStandbyDwell && time.Since(w.lastResume) >= resumeDebounce
		if fire {
			w.lastResume = time.Now()
		}
		w.mu.Unlock()
		switch {
		case fire:
			log.Printf("[resume] power-on detected (%s -> %s, standby %.0fs) — resuming", prev, src, dwell.Seconds())
			go w.onResume()
		case powerOn:
			log.Printf("[resume] power-on (%s -> %s) ignored (standby only %.0fs / debounced)", prev, src, dwell.Seconds())
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

	if err := ws.Connect(); err != nil {
		return err // initial connect failed (firmware WebSocket not ready yet)
	}
	// Block here; the library's internal reconnect loop handles drops. Wait
	// runs in its own goroutine because Disconnect is a no-op mid-reconnect,
	// which would otherwise leave shutdown stuck behind it.
	closed := make(chan struct{})
	go func() { ws.Wait(); close(closed) }()
	select {
	case <-closed:
		return nil
	case <-ctx.Done():
		_ = ws.Disconnect() // send a close frame rather than just vanishing
		return ctx.Err()
	}
}
