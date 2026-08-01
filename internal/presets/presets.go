// Package presets handles the on-device station configuration, persisted as JSON
// on the speaker's writable /mnt/nv partition.
package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
)

type Preset struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	StreamURL string `json:"stream_url"`
}

type Config struct {
	DeviceHost   string   `json:"device_host"`         // "" → 127.0.0.1 (we run on the speaker)
	ProxyPort    int      `json:"proxy_port"`          // local stream-proxy/control port
	APIToken     string   `json:"api_token,omitempty"` // "" → no auth; else required as Bearer token on mutating endpoints
	LastPresetID int      `json:"last_preset_id"`      // for auto-resume
	Presets      []Preset `json:"presets"`
}

// Clone returns a deep copy, so a mutated copy can be swapped in without
// racing readers that hold the old snapshot.
func (c *Config) Clone() *Config {
	out := *c
	out.Presets = append([]Preset(nil), c.Presets...)
	return &out
}

// Default returns the starter configuration (mirrors the SoundTouch-Pi presets).
func Default() *Config {
	return &Config{
		DeviceHost: "",
		ProxyPort:  8099,
		Presets: []Preset{
			{1, "BBC Radio 4", "http://stream.live.vc.bbcmedia.co.uk/bbc_radio_four_fm"},
			{2, "BBC Radio 6 Music", "http://stream.live.vc.bbcmedia.co.uk/bbc_6music"},
			{3, "NTS Radio 1", "https://stream-relay-geo.ntslive.net/stream"},
			{4, "KEXP Seattle", "https://kexp-mp3-128.streamguys1.com/kexp128.mp3"},
			{5, "Jazz24", "https://live.jazz24.org/jazz24"},
			{6, "Empty Preset 6", ""},
		},
	}
}

// Validate reports whether a config is safe to run with. A config that fails
// here would either crash the daemon at startup (an unusable proxy_port) or
// silently misbehave (a preset the speaker's 6 slots can't hold).
func Validate(c *Config) error {
	if c == nil {
		return fmt.Errorf("nil config")
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return fmt.Errorf("proxy_port %d out of range", c.ProxyPort)
	}
	if len(c.Presets) == 0 {
		return fmt.Errorf("no presets")
	}
	seen := make(map[int]bool, len(c.Presets))
	for _, p := range c.Presets {
		if p.ID < 1 || p.ID > 6 {
			return fmt.Errorf("preset id %d out of range (1-6)", p.ID)
		}
		// Duplicates would silently shadow each other: ByID returns the first,
		// but syncHardwarePresets writes every one into the same physical slot.
		if seen[p.ID] {
			return fmt.Errorf("duplicate preset id %d", p.ID)
		}
		seen[p.ID] = true
		if p.StreamURL != "" {
			u, err := url.Parse(p.StreamURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("preset %d: stream_url must be an http(s) URL", p.ID)
			}
		}
	}
	return nil
}

// Load reads config from path. If the file is absent, corrupt, empty, or fails
// Validate it falls back to path+".bak" (written by Save after every successful
// write) and finally to Default(). This means neither a power-cut during Save
// nor a bad hand-edit bricks the daemon — it self-heals on the next boot.
//
// Which candidate won is always logged: a silent fall back to Default() looks
// exactly like "my presets vanished", and the next config write would persist
// those defaults over the .bak that might still hold the real ones.
func Load(path string) (*Config, error) {
	for _, candidate := range []string{path, path + ".bak"} {
		data, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
			continue
		}
		if err != nil {
			log.Printf("[presets] %s unreadable (%v) — trying next candidate", candidate, err)
			continue
		}
		var c Config
		if err := json.Unmarshal(data, &c); err != nil {
			log.Printf("[presets] %s is corrupt (%v) — trying next candidate", candidate, err)
			continue
		}
		if c.ProxyPort == 0 {
			c.ProxyPort = 8099
		}
		if err := Validate(&c); err != nil {
			log.Printf("[presets] %s is invalid (%v) — trying next candidate", candidate, err)
			continue
		}
		log.Printf("[presets] loaded config from %s (%d presets)", candidate, len(c.Presets))
		return &c, nil
	}
	log.Printf("[presets] no usable config at %s or %s.bak — starting from defaults", path, path)
	return Default(), nil
}

// Save atomically writes the config to path (temp file + rename) and keeps a
// .bak copy so Load can recover if the primary file is ever corrupt.
func (c *Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	serr := f.Sync() // flush to storage before rename so a power-cut can't zero the file
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	if cerr != nil {
		return cerr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// Best-effort backup — Load() uses this if the primary file is ever corrupt.
	_ = os.WriteFile(path+".bak", data, 0o644)
	return nil
}

// ByID returns the preset with the given id, or nil.
func (c *Config) ByID(id int) *Preset {
	for i := range c.Presets {
		if c.Presets[i].ID == id {
			return &c.Presets[i]
		}
	}
	return nil
}
