package presets

import (
	"path/filepath"
	"testing"
)

func TestDefaultHasSixPresets(t *testing.T) {
	if n := len(Default().Presets); n != 6 {
		t.Fatalf("default presets = %d, want 6", n)
	}
}

func TestLoadMissingReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Presets) != 6 || c.ProxyPort != 8091 {
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
