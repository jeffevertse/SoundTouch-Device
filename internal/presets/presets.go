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
	Icon      string `json:"icon"`
}

type Config struct {
	DeviceHost   string   `json:"device_host"`    // "" → 127.0.0.1 (we run on the speaker)
	ProxyPort    int      `json:"proxy_port"`     // local stream-proxy/control port
	LastPresetID int      `json:"last_preset_id"` // for auto-resume
	Presets      []Preset `json:"presets"`
}

// Default returns the starter configuration (mirrors the SoundTouch-Pi presets).
func Default() *Config {
	return &Config{
		DeviceHost: "",
		ProxyPort:  8091,
		Presets: []Preset{
			{1, "BBC Radio 4", "http://stream.live.vc.bbcmedia.co.uk/bbc_radio_four_fm", "📻"},
			{2, "BBC Radio 6 Music", "http://stream.live.vc.bbcmedia.co.uk/bbc_6music", "🎵"},
			{3, "NTS Radio 1", "https://stream-relay-geo.ntslive.net/stream", "🎶"},
			{4, "KEXP Seattle", "https://kexp-mp3-128.streamguys1.com/kexp128.mp3", "🌲"},
			{5, "Jazz24", "https://live.jazz24.org/jazz24", "🎷"},
			{6, "Empty Preset 6", "", "⭕"},
		},
	}
}

// Load reads config from path, returning defaults if the file does not exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.ProxyPort == 0 {
		c.ProxyPort = 8091
	}
	return &c, nil
}

// Save atomically writes the config to path (temp file + rename).
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
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
