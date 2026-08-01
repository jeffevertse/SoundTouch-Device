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

// Validation itself is covered by presets.TestValidate; what matters here is
// that Replace enforces it.
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

func TestSameSiteOK(t *testing.T) {
	req := func(set map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/play/1", nil)
		for k, v := range set {
			r.Header.Set(k, v)
		}
		return r
	}

	// Non-browser clients (curl, the iOS app, the speaker's own renderer) send
	// no Fetch-Metadata headers and must keep working.
	if !sameSiteOK(req(nil)) {
		t.Error("a request with no Fetch-Metadata headers should be allowed")
	}
	// The editor page and user-initiated navigations.
	for _, site := range []string{"same-origin", "same-site", "none"} {
		if !sameSiteOK(req(map[string]string{"Sec-Fetch-Site": site})) {
			t.Errorf("Sec-Fetch-Site: %s should be allowed", site)
		}
	}
	// A public page doing <img src="http://speaker:8099/play/1">.
	if sameSiteOK(req(map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "no-cors",
		"Sec-Fetch-Dest": "image",
	})) {
		t.Error("cross-site request should be rejected")
	}
	// Belt and braces for clients that send Origin but not Sec-Fetch-Site.
	if sameSiteOK(req(map[string]string{"Origin": "https://evil.example.com"})) {
		t.Error("public Origin should be rejected")
	}
	if !sameSiteOK(req(map[string]string{"Origin": "http://192.168.1.50"})) {
		t.Error("LAN Origin should be allowed")
	}
}

// Every endpoint gets CORS now, not just the three that called cors() directly —
// a LAN dashboard needs /status and /healthz too.
func TestWithCORSCoversAllEndpointsAndPreflight(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	h := withCORS(mux)

	r := httptest.NewRequest("GET", "/status", nil)
	r.Header.Set("Origin", "http://192.168.1.50")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://192.168.1.50" {
		t.Errorf("/status Allow-Origin = %q, want the LAN origin", got)
	}

	// Preflight is answered without reaching the handler.
	r = httptest.NewRequest("OPTIONS", "/status", nil)
	r.Header.Set("Origin", "http://192.168.1.50")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("preflight should have an empty body, got %q", w.Body.String())
	}

	// A public origin still gets nothing.
	r = httptest.NewRequest("GET", "/status", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("public origin should not get a CORS header")
	}
}
