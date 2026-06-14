package resume

import "testing"

func TestPresetIDRe(t *testing.T) {
	frame := `<updates deviceID="X"><nowSelectionUpdated><preset id="3" sourceAccount="">` +
		`<ContentItem source="LOCAL_INTERNET_RADIO"/></preset></nowSelectionUpdated></updates>`
	m := presetIDRe.FindStringSubmatch(frame)
	if m == nil || m[1] != "3" {
		t.Fatalf("expected preset 3, got %v", m)
	}
	// A nowPlayingUpdated frame (no selection) must not match.
	if presetIDRe.FindStringSubmatch(
		`<updates><nowPlayingUpdated><ContentItem/></nowPlayingUpdated></updates>`) != nil {
		t.Error("should not match nowPlayingUpdated frames")
	}
}
