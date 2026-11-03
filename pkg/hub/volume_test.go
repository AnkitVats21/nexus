package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A hub speaker's volume: the player reports it (volume_state), the hub
// shows it in music_state and /hub/status, and the dashboard sets it
// (speaker_volume on /api/ws), which reaches the player as music_volume.
func TestSpeakerVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := newMusicHub(t, nil)
	// The dashboard's socket needs a board with the state role.
	roles := map[string]any{"state": map[string]any{"http_port": 80}}
	for k, v := range boardRoles {
		roles[k] = v
	}
	board := connectEdge(t, ctx, srv.URL, "board", roles)
	orbit := connectEdge(t, ctx, srv.URL, "workstation", orbitRoles)

	write(t, ctx, orbit.conn, map[string]any{"type": "volume_state", "volume": 44})
	for {
		st := board.next(ctx, "music_state")
		var vol any
		for _, p := range st["players"].([]any) {
			if pm := p.(map[string]any); pm["id"] == "workstation" {
				vol = pm["volume"]
			}
		}
		if vol == float64(44) {
			break
		}
	}
	resp, err := http.Get(srv.URL + "/hub/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Edges []map[string]any `json:"edges"`
	}
	json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	var found bool
	for _, e := range status.Edges {
		if e["id"] == "workstation" {
			found = e["volume"] == float64(44)
		} else if _, ok := e["volume"]; ok {
			t.Fatalf("a volume for %v, which reported none", e["id"])
		}
	}
	if !found {
		t.Fatalf("status edges: %v", status.Edges)
	}

	// The dashboard, by the speaker's name; out of range is clamped.
	br := dial(t, ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/ws")
	write(t, ctx, br, map[string]any{"cmd": "speaker_volume", "target": "workstation", "value": 130})
	if m := orbit.next(ctx, "music_volume"); m["volume"] != float64(100) {
		t.Fatalf("music_volume: %v", m)
	}
	// Not for the board (its volume is its own); no music_volume to it.
	write(t, ctx, br, map[string]any{"cmd": "speaker_volume", "target": "board", "value": 10})
	write(t, ctx, br, map[string]any{"cmd": "speaker_volume", "target": "workstation", "value": 30})
	if m := orbit.next(ctx, "music_volume"); m["volume"] != float64(30) {
		t.Fatalf("music_volume: %v", m)
	}
}
