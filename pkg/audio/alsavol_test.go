package audio

import "testing"

func TestPickALSAControl(t *testing.T) {
	cases := []struct {
		name, out, want string
		wantErr         bool
	}{
		{"a2dp", "Simple mixer control 'Tribit StormBox 2 A2DP',0\n", "Tribit StormBox 2 A2DP", false},
		{"a2dp among others", "Simple mixer control 'Phone SCO',0\nSimple mixer control 'Speaker - A2DP',0\n", "Speaker - A2DP", false},
		{"only one", "Simple mixer control 'Master',0\n", "Master", false},
		{"index", "Simple mixer control 'Spk A2DP',1\n", "Spk A2DP,1", false},
		{"none", "", "", true},
		{"ambiguous", "Simple mixer control 'A',0\nSimple mixer control 'B',0\n", "", true},
	}
	for _, c := range cases {
		got, err := pickALSAControl(c.out)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: got %q, %v; want %q (err %v)", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestParseAmixerVolume(t *testing.T) {
	out := `Simple mixer control 'Tribit StormBox 2 A2DP',0
  Capabilities: pvolume pswitch
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 127
  Mono:
  Front Left: Playback 76 [60%] [-12.00dB] [on]
  Front Right: Playback 76 [60%] [-12.00dB] [on]
`
	if v, err := parseAmixerVolume(out); err != nil || v != 60 {
		t.Fatalf("got %d, %v; want 60", v, err)
	}
	off := "  Front Left: Playback 76 [60%] [off]\n"
	if v, err := parseAmixerVolume(off); err != nil || v != 0 {
		t.Fatalf("muted: got %d, %v; want 0", v, err)
	}
	if _, err := parseAmixerVolume("Simple mixer control 'x',0\n"); err == nil {
		t.Fatal("no level: want an error")
	}
}
