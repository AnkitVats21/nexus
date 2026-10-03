package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nexus/config"
	"nexus/pkg/protocol"
	"nexus/pkg/resolver"
	"nexus/pkg/transport"
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

// TestOrbitPairingFlow tests that when a pair code is configured:
// 1. Hello message sends proto 3 and pair_code, without a token.
// 2. On "claimed", token is saved to OrbitTokenFile with mode 0600 (parent dir 0700).
// 3. Reconnection uses the saved token instead of the pair code.
func TestOrbitPairingFlow(t *testing.T) {
	tmpDir := t.TempDir()
	tokFile := filepath.Join(tmpDir, "nested", "orbit_token")

	type hubStep int
	const (
		stepInitial hubStep = iota
		stepReconnected
	)

	stepCh := make(chan hubStep, 10)
	helloReceived := make(chan map[string]any, 10)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var hello map[string]any
		_ = json.Unmarshal(data, &hello)
		helloReceived <- hello

		if _, hasToken := hello["token"]; !hasToken {
			// Step 1: Pair code sent, reply with claimed then welcome
			stepCh <- stepInitial
			claimedMsg, _ := json.Marshal(map[string]any{
				"type":  "claimed",
				"token": "tok-orbit-secret-xyz",
				"owner": "Alice",
			})
			_ = conn.Write(r.Context(), websocket.MessageText, claimedMsg)

			welcomeMsg, _ := json.Marshal(map[string]any{
				"type":    "welcome",
				"claimed": true,
				"owner":   "Alice",
			})
			_ = conn.Write(r.Context(), websocket.MessageText, welcomeMsg)
		} else {
			// Step 2: Reconnection with token
			stepCh <- stepReconnected
			welcomeMsg, _ := json.Marshal(map[string]any{
				"type": "welcome",
			})
			_ = conn.Write(r.Context(), websocket.MessageText, welcomeMsg)
		}

		// Keep connection alive until client disconnects or context ends
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")

	cfg := &config.Config{
		SatelliteID:    "orbit-device-1",
		SatelliteName:  "Test Orbit",
		HubURL:         wsURL,
		PairCode:       "482913",
		OrbitTokenFile: tokFile,
	}

	trans := transport.NewWebSocketTransport()
	defer trans.Close()

	coord := NewCoordinator(cfg, trans, resolver.NewDirectResolver(), &mockPlayer{})
	if err := coord.Start(); err != nil {
		t.Fatal(err)
	}
	defer coord.Stop()

	if err := trans.Connect(context.Background(), wsURL); err != nil {
		t.Fatal(err)
	}

	// 1. Verify first hello carries proto 3 and pair_code, no token
	select {
	case h := <-helloReceived:
		if h["proto"] != float64(3) {
			t.Fatalf("expected proto 3, got %v", h["proto"])
		}
		if h["pair_code"] != "482913" {
			t.Fatalf("expected pair_code 482913, got %v", h["pair_code"])
		}
		if tok, exists := h["token"]; exists && tok != "" {
			t.Fatalf("expected no token in first hello, got %v", tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for initial hello")
	}

	// Wait for stepInitial
	select {
	case st := <-stepCh:
		if st != stepInitial {
			t.Fatalf("unexpected step %v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for claimed message")
	}

	// Wait briefly for token to be saved to file
	var info os.FileInfo
	var readToken string
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(tokFile)
		if err == nil {
			readToken = strings.TrimSpace(string(data))
			info, _ = os.Stat(tokFile)
			break
		}
	}
	if readToken != "tok-orbit-secret-xyz" {
		t.Fatalf("expected token tok-orbit-secret-xyz in file, got %q", readToken)
	}
	if info == nil || info.Mode().Perm() != 0600 {
		t.Fatalf("expected token file mode 0600, got %v", info)
	}
	pinfo, err := os.Stat(filepath.Dir(tokFile))
	if err != nil {
		t.Fatalf("parent dir stat: %v", err)
	}
	if pinfo.Mode().Perm() != 0700 {
		t.Fatalf("expected token dir mode 0700, got %o", pinfo.Mode().Perm())
	}

	if cfg.OrbitToken != "tok-orbit-secret-xyz" {
		t.Fatalf("in-memory OrbitToken not updated: %q", cfg.OrbitToken)
	}
	if cfg.PairCode != "" {
		t.Fatalf("in-memory PairCode not cleared: %q", cfg.PairCode)
	}

	// 2. Reconnect: close previous connection, start new transport
	trans.Close()

	trans2 := transport.NewWebSocketTransport()
	defer trans2.Close()

	coord2 := NewCoordinator(cfg, trans2, resolver.NewDirectResolver(), &mockPlayer{})
	if err := coord2.Start(); err != nil {
		t.Fatal(err)
	}
	defer coord2.Stop()

	if err := trans2.Connect(context.Background(), wsURL); err != nil {
		t.Fatal(err)
	}

	select {
	case h := <-helloReceived:
		if h["proto"] != float64(3) {
			t.Fatalf("expected proto 3, got %v", h["proto"])
		}
		if h["token"] != "tok-orbit-secret-xyz" {
			t.Fatalf("expected reconnect token tok-orbit-secret-xyz, got %v", h["token"])
		}
		if code, exists := h["pair_code"]; exists && code != "" {
			t.Fatalf("expected no pair_code on reconnect, got %v", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reconnect hello")
	}

	select {
	case st := <-stepCh:
		if st != stepReconnected {
			t.Fatalf("unexpected step %v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reconnected welcome")
	}
}

// TestOrbitBadTokenFlow tests that when a hub rejects with "bad token",
// the stored orbit token file is deleted and the in-memory token is cleared.
func TestOrbitBadTokenFlow(t *testing.T) {
	tmpDir := t.TempDir()
	tokFile := filepath.Join(tmpDir, "orbit_token")
	if err := config.SaveOrbitToken(tokFile, "invalid-token-123"); err != nil {
		t.Fatal(err)
	}

	helloReceived := make(chan map[string]any, 10)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var hello map[string]any
		_ = json.Unmarshal(data, &hello)
		helloReceived <- hello

		rejMsg, _ := json.Marshal(map[string]any{
			"type":   "rejected",
			"reason": "bad token",
		})
		_ = conn.Write(r.Context(), websocket.MessageText, rejMsg)

		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")

	cfg := &config.Config{
		SatelliteID:    "orbit-device-bad",
		SatelliteName:  "Test Orbit Bad",
		HubURL:         wsURL,
		OrbitTokenFile: tokFile,
		OrbitToken:     "invalid-token-123",
	}

	trans := transport.NewWebSocketTransport()
	defer trans.Close()

	coord := NewCoordinator(cfg, trans, resolver.NewDirectResolver(), &mockPlayer{})
	if err := coord.Start(); err != nil {
		t.Fatal(err)
	}
	defer coord.Stop()

	if err := trans.Connect(context.Background(), wsURL); err != nil {
		t.Fatal(err)
	}

	select {
	case h := <-helloReceived:
		if h["token"] != "invalid-token-123" {
			t.Fatalf("expected token invalid-token-123, got %v", h["token"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for hello")
	}

	// Verify file is deleted and in-memory token is cleared
	var fileDeleted bool
	for start := time.Now(); time.Since(start) < 2*time.Second; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(tokFile); os.IsNotExist(err) {
			fileDeleted = true
			break
		}
	}
	if !fileDeleted {
		t.Fatal("expected orbit token file to be deleted on bad token")
	}
	if cfg.OrbitToken != "" {
		t.Fatalf("expected OrbitToken to be cleared, got %q", cfg.OrbitToken)
	}
}

// TestOrbitLegacyTokenFlow tests that when no device token or pair code is set,
// the legacy hub token is sent in hello, and no orbit token file is created.
func TestOrbitLegacyTokenFlow(t *testing.T) {
	tmpDir := t.TempDir()
	legacyFile := filepath.Join(tmpDir, "hub_tokens")
	tokFile := filepath.Join(tmpDir, "orbit_token")
	if err := os.WriteFile(legacyFile, []byte("shared-legacy-token-999\n"), 0600); err != nil {
		t.Fatal(err)
	}

	helloReceived := make(chan map[string]any, 10)

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")

		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var hello map[string]any
		_ = json.Unmarshal(data, &hello)
		helloReceived <- hello

		welcomeMsg, _ := json.Marshal(map[string]any{
			"type": "welcome",
		})
		_ = conn.Write(r.Context(), websocket.MessageText, welcomeMsg)

		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")

	cfg := &config.Config{
		SatelliteID:    "orbit-device-legacy",
		SatelliteName:  "Test Orbit Legacy",
		HubURL:         wsURL,
		OrbitTokenFile: tokFile,
		HubTokenFile:   legacyFile,
		HubToken:       "shared-legacy-token-999",
	}

	trans := transport.NewWebSocketTransport()
	defer trans.Close()

	coord := NewCoordinator(cfg, trans, resolver.NewDirectResolver(), &mockPlayer{})
	if err := coord.Start(); err != nil {
		t.Fatal(err)
	}
	defer coord.Stop()

	if err := trans.Connect(context.Background(), wsURL); err != nil {
		t.Fatal(err)
	}

	select {
	case h := <-helloReceived:
		if h["token"] != "shared-legacy-token-999" {
			t.Fatalf("expected legacy token shared-legacy-token-999, got %v", h["token"])
		}
		if code, exists := h["pair_code"]; exists && code != "" {
			t.Fatalf("expected no pair_code, got %v", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for hello")
	}

	// Verify orbit_token file was NOT created
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatalf("orbit_token file should not exist, err=%v", err)
	}
}
