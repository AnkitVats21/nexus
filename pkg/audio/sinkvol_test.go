package audio

import "testing"

func TestParseWpctlVolume(t *testing.T) {
	for out, want := range map[string]int{
		"Volume: 0.40\n":         40,
		"Volume: 0.44 [MUTED]\n": 0,
		"Volume: 1.00\n":         100,
		"Volume: 0.075\n":        8,
	} {
		if got, err := parseWpctlVolume(out); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", out, got, err, want)
		}
	}
	if _, err := parseWpctlVolume("Node '60' not found"); err == nil {
		t.Error("no error for a missing node")
	}
}
