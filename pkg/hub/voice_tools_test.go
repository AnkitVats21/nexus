package hub

import (
	"encoding/json"
	"log/slog"
	"testing"
)

// A music question (music_status, a speaker listing) doesn't end the
// session; a command does.
func TestMusicToolActs(t *testing.T) {
	cases := []struct {
		name, args string
		want       bool
	}{
		{"play", `{"query":"x"}`, true},
		{"playback", `{"action":"pause"}`, true},
		{"music_output", `{"target":"here"}`, true},
		{"music_output", `{}`, false},
		{"music_status", `{}`, false},
	}
	for _, c := range cases {
		if got := musicToolActs(c.name, json.RawMessage(c.args)); got != c.want {
			t.Errorf("musicToolActs(%s, %s) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func TestMusicStatusTool(t *testing.T) {
	m := &Music{log: slog.Default()}
	var res map[string]any
	json.Unmarshal(m.statusTool(), &res)
	if res["status"] != "success" || res["now_playing"] != nil || res["queue_length"].(float64) != 0 {
		t.Fatalf("idle status: %v", res)
	}

	m.cur = &Track{ID: "a", Title: "Alpha", Artist: "X"}
	m.state, m.posMs, m.durMs = "playing", 61000, 200000
	m.queue = []Track{{Title: "Beta", Artist: "Y"}, {Title: "Gamma", Artist: "Z"}}
	res = nil
	json.Unmarshal(m.statusTool(), &res)
	np := res["now_playing"].(map[string]any)
	if np["title"] != "Alpha" || np["position_s"].(float64) != 61 || np["duration_s"].(float64) != 200 {
		t.Fatalf("now_playing: %v", np)
	}
	next := res["up_next"].([]any)
	if len(next) != 2 || next[0] != "Beta by Y" || res["queue_length"].(float64) != 2 {
		t.Fatalf("up_next: %v", res)
	}
}

func TestMCPToolKind(t *testing.T) {
	for name, want := range map[string]string{"web_search": "search", "news_headlines": "search", "weather": "info", "stock_quote": "other"} {
		if got := mcpToolKind(name); got != want {
			t.Errorf("mcpToolKind(%s) = %s, want %s", name, got, want)
		}
	}
}
