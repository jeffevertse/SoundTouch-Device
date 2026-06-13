// Package resume re-plays the last station when the speaker powers on, using the
// SoundTouch WebSocket event stream (via the gesellix client).
package resume

import (
	"log"
	"strings"
	"sync"

	"github.com/gesellix/bose-soundtouch/pkg/client"
	"github.com/gesellix/bose-soundtouch/pkg/models"
)

// Watcher detects STANDBY→on transitions and invokes play().
type Watcher struct {
	client *client.Client
	play   func()
	mu     sync.Mutex
	prev   string
}

func New(c *client.Client, play func()) *Watcher {
	return &Watcher{client: c, play: play}
}

// Start connects the WebSocket and watches for power-on. The gesellix client
// auto-reconnects, so this returns after the initial connect.
func (w *Watcher) Start() error {
	ws := w.client.NewWebSocketClient(client.DefaultWebSocketConfig())
	ws.OnNowPlaying(func(ev *models.NowPlayingUpdatedEvent) {
		src := strings.ToUpper(strings.TrimSpace(ev.NowPlaying.Source))
		w.mu.Lock()
		prev := w.prev
		w.prev = src
		w.mu.Unlock()
		if prev == "STANDBY" && src != "" && src != "STANDBY" {
			log.Printf("[resume] power-on detected (%s -> %s) — resuming", prev, src)
			go w.play()
		}
	})
	return ws.Connect()
}
