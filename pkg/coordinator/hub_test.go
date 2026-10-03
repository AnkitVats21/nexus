package coordinator

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nexus/config"
	"nexus/pkg/protocol"
	"nexus/pkg/resolver"
)

// Hub mode: hello with the player role, the hub's URL played as sent, and
// reports and events carry the music_play's seq.
func TestHubMode(t *testing.T) {
	trans := newMockTransport()
	player := &mockPlayer{}
	coord := NewCoordinator(&config.Config{SatelliteID: "pc", SatelliteName: "Workstation",
		HubURL: "ws://hub/hub/edge", HubToken: "tok"}, trans, resolver.NewDirectResolver(), player)
	if err := coord.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coord.Stop)

	coord.handleTransportConnected()
	send(trans, map[string]any{"type": "music_play", "seq": 7, "position_ms": 3000,
		"url": "https://cdn.test/a", "track": map[string]any{"id": "a", "title": "Alpha"}})
	time.Sleep(50 * time.Millisecond)
	player.mu.Lock()
	if player.loadedURL != "https://cdn.test/a" || player.loadedPos != 3000 {
		t.Fatalf("loaded %q at %d", player.loadedURL, player.loadedPos)
	}
	ended := player.onTrackEnded
	player.mu.Unlock()
	coord.emitTelemetryReport()
	send(trans, map[string]any{"type": "music_pause"})
	player.mu.Lock()
	paused := player.paused
	player.mu.Unlock()
	if !paused {
		t.Fatal("music_pause not applied")
	}
	ended(nil)

	trans.mu.Lock()
	defer trans.mu.Unlock()
	var hello, report, event map[string]any
	for _, m := range trans.sentMsgs {
		var env map[string]any
		_ = json.Unmarshal(m, &env)
		switch env["type"] {
		case "hello":
			hello = env
		case "music_report":
			report = env
		case "music_event":
			event = env
		}
	}
	roles, _ := hello["roles"].(map[string]any)
	if hello["token"] != "tok" || hello["kind"] != "orbit" || roles["player"] == nil {
		t.Fatalf("hello: %v", hello)
	}
	if report["seq"] != float64(7) || report["id"] != "a" || report["state"] != "playing" {
		t.Fatalf("report: %v", report)
	}
	if event["seq"] != float64(7) || event["event"] != "ended" || event["id"] != "a" {
		t.Fatalf("event: %v", event)
	}
}

// The hub link drops: the satellite's music stops.
func TestHubLostStops(t *testing.T) {
	trans := newMockTransport()
	player := &mockPlayer{}
	coord := NewCoordinator(&config.Config{SatelliteID: "pc", HubURL: "ws://hub/hub/edge"}, trans,
		resolver.NewDirectResolver(), player)
	if err := coord.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coord.Stop)
	send(trans, map[string]any{"type": "music_play", "seq": 1, "url": "https://cdn.test/a",
		"track": map[string]any{"id": "a"}})
	time.Sleep(30 * time.Millisecond)
	trans.errCh <- errors.New("read error: EOF")
	time.Sleep(30 * time.Millisecond)
	player.mu.Lock()
	defer player.mu.Unlock()
	if player.status.State != protocol.StateStopped {
		t.Fatalf("state %v, want stopped", player.status.State)
	}
}
