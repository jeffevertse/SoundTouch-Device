package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	bad = presets.Default()
	bad.Presets[0].StreamURL = "ftp://example.com/stream"
	if validateConfig(bad) == nil {
		t.Error("non-http(s) stream_url should be rejected")
	}
	bad = presets.Default()
	bad.Presets[0].StreamURL = "not a url"
	if validateConfig(bad) == nil {
		t.Error("unparseable stream_url should be rejected")
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

	// Snapshots handed out before the change must not be mutated (copy-on-write).
	snap := s.Get()
	s.SetLastPreset(5)
	if snap.LastPresetID != 4 {
		t.Errorf("old snapshot mutated: LastPresetID = %d, want 4", snap.LastPresetID)
	}

	// Unchanged id must not rewrite the file (flash wear on /mnt/nv).
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config should exist: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	s.SetLastPreset(5)
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("unchanged last preset should not rewrite the config file")
	}
}

func TestRequireMutation(t *testing.T) {
	cfg := presets.Default()

	post := func(contentType, auth string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/config", strings.NewReader("{}"))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		requireMutation(w, r, cfg)
		return w
	}

	// No token configured: JSON Content-Type is still required (CSRF guard).
	if w := post("", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("missing Content-Type: got %d, want 415", w.Code)
	}
	if w := post("text/plain", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain (form CSRF vector): got %d, want 415", w.Code)
	}
	if w := post("application/json; charset=utf-8", ""); w.Code != http.StatusOK {
		t.Errorf("JSON without token requirement: got %d, want 200", w.Code)
	}

	// Token configured: bearer required on top of the Content-Type.
	cfg.APIToken = "s3cret"
	if w := post("application/json", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer: got %d, want 401", w.Code)
	}
	if w := post("application/json", "Bearer wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer: got %d, want 401", w.Code)
	}
	if w := post("application/json", "Bearer s3cret"); w.Code != http.StatusOK {
		t.Errorf("correct bearer: got %d, want 200", w.Code)
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
	// Origin "null" (sandboxed iframes on public sites, file:// pages) must NOT
	// be allowed — the editor is served same-origin from GET / instead.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/config", nil)
	r.Header.Set("Origin", "null")
	cors(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("Origin null should not be allowed")
	}

	// a LAN origin gets CORS (e.g. a Home Assistant dashboard)
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/config", nil)
	r.Header.Set("Origin", "http://192.168.1.50")
	cors(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "http://192.168.1.50" {
		t.Error("LAN origin should be reflected")
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
