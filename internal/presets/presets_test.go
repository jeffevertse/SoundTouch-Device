package presets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultHasSixPresets(t *testing.T) {
	if n := len(Default().Presets); n != 6 {
		t.Fatalf("default presets = %d, want 6", n)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(Default()); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
	if Validate(nil) == nil {
		t.Error("nil config should be rejected")
	}
	bad := Default()
	bad.ProxyPort = 0
	if Validate(bad) == nil {
		t.Error("port 0 should be rejected")
	}
	bad = Default()
	bad.ProxyPort = 99999
	if Validate(bad) == nil {
		t.Error("port 99999 should be rejected")
	}
	bad = Default()
	bad.Presets = nil
	if Validate(bad) == nil {
		t.Error("no presets should be rejected")
	}
	bad = Default()
	bad.Presets[0].ID = 9
	if Validate(bad) == nil {
		t.Error("preset id 9 should be rejected")
	}
	bad = Default()
	bad.Presets[1].ID = 1
	if Validate(bad) == nil {
		t.Error("duplicate preset id should be rejected")
	}
	bad = Default()
	bad.Presets[0].StreamURL = "ftp://example.com/stream"
	if Validate(bad) == nil {
		t.Error("non-http(s) stream_url should be rejected")
	}
	bad = Default()
	bad.Presets[0].StreamURL = "not a url"
	if Validate(bad) == nil {
		t.Error("unparseable stream_url should be rejected")
	}
}

// A config that parses but can't be run (here: a port that would make
// ListenAndServe fail) must fall through to the .bak rather than boot-loop the
// daemon.
func TestLoadSkipsInvalidPrimaryAndUsesBak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	good := Default()
	good.Presets[0].Name = "From Backup"
	if err := good.Save(path); err != nil { // Save writes both config.json and .bak
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"proxy_port":99999,"presets":[{"id":1}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProxyPort != 8099 || c.ByID(1).Name != "From Backup" {
		t.Errorf("expected the .bak config, got port %d preset %+v", c.ProxyPort, c.ByID(1))
	}
}

func TestLoadCorruptEverywhereReturnsDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte("also not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Presets) != 6 || c.ProxyPort != 8099 {
		t.Errorf("expected defaults, got %d presets port %d", len(c.Presets), c.ProxyPort)
	}
}

func TestLoadMissingReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Presets) != 6 || c.ProxyPort != 8099 {
		t.Fatalf("expected defaults, got %d presets port %d", len(c.Presets), c.ProxyPort)
	}
}

func TestSaveLoadRoundTripAndByID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	in := Default()
	in.LastPresetID = 3
	in.Presets[0].Name = "My Station"
	if err := in.Save(path); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.LastPresetID != 3 {
		t.Errorf("LastPresetID = %d, want 3", out.LastPresetID)
	}
	if p := out.ByID(1); p == nil || p.Name != "My Station" {
		t.Errorf("ByID(1) = %+v", p)
	}
	if out.ByID(99) != nil {
		t.Error("ByID(99) should be nil")
	}
}
