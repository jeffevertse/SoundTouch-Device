package main

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jeffevertse/soundtouch-device/internal/presets"
)

func TestValidateConfig(t *testing.T) {
	if err := validateConfig(presets.Default()); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
	bad := presets.Default()
	bad.ProxyPort = 0
	if validateConfig(bad) == nil {
		t.Error("port 0 should be rejected")
	}
	bad = presets.Default()
	bad.Presets = nil
	if validateConfig(bad) == nil {
		t.Error("no presets should be rejected")
	}
	bad = presets.Default()
	bad.Presets[0].ID = 9
	if validateConfig(bad) == nil {
		t.Error("preset id 9 should be rejected")
	}
}

func TestConfigStoreReplaceAndGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := &configStore{cfg: presets.Default(), path: path}

	// invalid replace is rejected and leaves the current config intact
	bad := presets.Default()
	bad.ProxyPort = -1
	if err := s.Replace(bad); err == nil {
		t.Error("invalid replace should error")
	}
	if s.Get().ProxyPort != 8099 {
		t.Error("config should be unchanged after a rejected replace")
	}

	// valid replace persists and swaps
	good := presets.Default()
	good.Presets[0].Name = "My Station"
	good.ProxyPort = 8099
	if err := s.Replace(good); err != nil {
		t.Fatalf("valid replace: %v", err)
	}
	if s.Get().ByID(1).Name != "My Station" {
		t.Error("Get should reflect the replaced config")
	}
	reloaded, err := presets.Load(path)
	if err != nil || reloaded.ByID(1).Name != "My Station" {
		t.Errorf("config should be persisted to disk: %v", err)
	}
}

func TestConfigStoreLastPreset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := &configStore{cfg: presets.Default(), path: path}
	s.SetLastPreset(4)
	if s.LastPreset() != 4 {
		t.Errorf("LastPreset = %d, want 4", s.LastPreset())
	}
}

func TestIsLocalOrigin(t *testing.T) {
	local := []string{"http://localhost:9000", "http://127.0.0.1:8099", "http://192.168.1.50", "http://10.0.0.2:3000"}
	for _, o := range local {
		if !isLocalOrigin(o) {
			t.Errorf("%s should be local", o)
		}
	}
	for _, o := range []string{"https://evil.example.com", "http://8.8.8.8", ""} {
		if isLocalOrigin(o) {
			t.Errorf("%s should NOT be local", o)
		}
	}
}

func TestCORS(t *testing.T) {
	// file:// pages send Origin: null → must be allowed
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/config", nil)
	r.Header.Set("Origin", "null")
	cors(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "null" {
		t.Error("Origin null should be reflected")
	}

	// a public website must NOT get a CORS header
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/config", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	cors(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("public origin should not be allowed")
	}
}
