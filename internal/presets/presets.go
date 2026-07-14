// Package presets handles the on-device station configuration, persisted as JSON
// on the speaker's writable /mnt/nv partition.
package presets

import (
	"encoding/json"
	"errors"
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

// Load reads config from path. If the file is absent, corrupt, or empty it
// falls back to path+".bak" (written by Save after every successful write)
// and finally to Default(). This means a power-cut during Save never bricks
// the daemon — it self-heals on the next boot.
func Load(path string) (*Config, error) {
	for _, candidate := range []string{path, path + ".bak"} {
		data, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
			continue
		}
		if err != nil {
			continue
		}
		var c Config
		if err := json.Unmarshal(data, &c); err != nil {
			continue // corrupt — try next candidate
		}
		if c.ProxyPort == 0 {
			c.ProxyPort = 8099
		}
		return &c, nil
	}
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
