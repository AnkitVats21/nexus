package coordinator

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"nexus-orbit/config"
	"nexus-orbit/pkg/audio"
	"nexus-orbit/pkg/protocol"
	"nexus-orbit/pkg/resolver"
)

type mockPlayer struct {
	mu           sync.Mutex
	status       audio.PlayerStatus
	loadedURL    string
	loadedPos    int64
	paused       bool
	resumed      bool
	seekPos      int64
	onTrackEnded func()
	onStatus     func(status audio.PlayerStatus)
}

func (m *mockPlayer) LoadURL(url string, startPosMs int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadedURL = url
	m.loadedPos = startPosMs
	m.status.State = protocol.StatePlaying
	return nil
}
func (m *mockPlayer) Pause() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused = true
	m.status.State = protocol.StatePaused
	return nil
}
func (m *mockPlayer) Resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumed = true
	m.status.State = protocol.StatePlaying
	return nil
}
func (m *mockPlayer) Seek(posMs int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seekPos = posMs
	m.status.PositionMs = posMs
	return nil
}
func (m *mockPlayer) SetVolume(vol int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Volume = vol
	return nil
}
func (m *mockPlayer) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.State = protocol.StateStopped
	return nil
}
func (m *mockPlayer) GetStatus() (audio.PlayerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}
func (m *mockPlayer) OnTrackEnded(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onTrackEnded = fn
}
func (m *mockPlayer) OnStatusChanged(fn func(status audio.PlayerStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStatus = fn
}
func (m *mockPlayer) Close() error { return nil }

type mockTransport struct {
	mu          sync.Mutex
	sentMsgs    [][]byte
	msgCh       chan []byte
	errCh       chan error
	onConnected func()
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		msgCh: make(chan []byte, 10),
		errCh: make(chan error, 10),
	}
}

func (t *mockTransport) Connect(ctx context.Context, endpoint string) error { return nil }
func (t *mockTransport) Send(msg []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sentMsgs = append(t.sentMsgs, msg)
	return nil
}
func (t *mockTransport) Receive() (<-chan []byte, <-chan error) {
	return t.msgCh, t.errCh
}
func (t *mockTransport) OnConnected(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onConnected = fn
}
func (t *mockTransport) Close() error { return nil }

func TestCoordinatorRouting(t *testing.T) {
	cfg := &config.Config{
		SatelliteID:   "test-sat",
		SatelliteName: "Test Sat",
	}

	trans := newMockTransport()
	res := resolver.NewDirectResolver()
	player := &mockPlayer{}

	coord := NewCoordinator(cfg, trans, res, player, nil)
	if err := coord.Start(); err != nil {
		t.Fatalf("failed to start coordinator: %v", err)
	}
	defer coord.Stop()

	// Simulate inbound orbit_route
	route := protocol.OrbitRouteMsg{
		Cmd:        "orbit_route",
		VideoID:    "test1234",
		PositionMs: 15000,
	}
	routeBytes, _ := json.Marshal(route)
	trans.msgCh <- routeBytes

	time.Sleep(100 * time.Millisecond)

	player.mu.Lock()
	if player.loadedPos != 15000 {
		t.Errorf("expected loadedPos 15000, got %d", player.loadedPos)
	}
	expectedURL := "https://www.youtube.com/watch?v=test1234"
	if player.loadedURL != expectedURL {
		t.Errorf("expected URL %s, got %s", expectedURL, player.loadedURL)
	}
	player.mu.Unlock()

	// Simulate inbound orbit_pause
	pauseMsg, _ := json.Marshal(protocol.BaseMessage{Cmd: "orbit_pause"})
	trans.msgCh <- pauseMsg

	time.Sleep(50 * time.Millisecond)

	player.mu.Lock()
	if !player.paused {
		t.Error("expected player to be paused")
	}
	player.mu.Unlock()
}
