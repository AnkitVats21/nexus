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
	onTrackEnded func(err error)
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
func (m *mockPlayer) SeekTo(posMs int64) error {
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
func (m *mockPlayer) SetMediaTitle(title, artist string) error {
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
func (m *mockPlayer) OnTrackEnded(fn func(err error)) {
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

	coord := NewCoordinator(cfg, trans, res, player)
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

// gatedResolver resolves only when the test releases it, to send commands
// while a play is still resolving.
type gatedResolver struct {
	release chan struct{}
}

func (r *gatedResolver) Resolve(ctx context.Context, videoID string) (*resolver.ResolvedTrack, error) {
	<-r.release
	return &resolver.ResolvedTrack{VideoID: videoID, PlaybackURL: "url-" + videoID}, nil
}
func (r *gatedResolver) Name() string { return "gated" }

func newTestCoordinator(t *testing.T, res resolver.Resolver) (*Coordinator, *mockTransport, *mockPlayer) {
	t.Helper()
	trans := newMockTransport()
	player := &mockPlayer{}
	coord := NewCoordinator(&config.Config{SatelliteID: "test-sat"}, trans, res, player)
	if err := coord.Start(); err != nil {
		t.Fatalf("failed to start coordinator: %v", err)
	}
	t.Cleanup(coord.Stop)
	return coord, trans, player
}

func send(trans *mockTransport, msg any) {
	b, _ := json.Marshal(msg)
	trans.msgCh <- b
	time.Sleep(30 * time.Millisecond)
}

// A pause sent right after a play (a song asked for during an assistant
// session) must hold once the track loads, not be lost while it resolves.
func TestPauseDuringResolveAppliesAfterLoad(t *testing.T) {
	res := &gatedResolver{release: make(chan struct{})}
	_, trans, player := newTestCoordinator(t, res)

	send(trans, protocol.OrbitRouteMsg{Cmd: "orbit_play", VideoID: "a", PositionMs: 5000})
	send(trans, protocol.BaseMessage{Cmd: "orbit_pause"})
	player.mu.Lock()
	player.paused = false // the immediate Pause() found nothing loaded
	player.mu.Unlock()
	close(res.release)
	time.Sleep(50 * time.Millisecond)

	player.mu.Lock()
	defer player.mu.Unlock()
	if player.loadedURL != "url-a" || player.loadedPos != 5000 {
		t.Fatalf("expected url-a at 5000, got %q at %d", player.loadedURL, player.loadedPos)
	}
	if !player.paused {
		t.Error("expected the pause to be applied after the load")
	}
}

// A stop, or a newer play, while a play resolves: the old play never loads.
func TestStopAndNewerPlayDropStaleResolve(t *testing.T) {
	res := &gatedResolver{release: make(chan struct{})}
	_, trans, player := newTestCoordinator(t, res)

	send(trans, protocol.OrbitRouteMsg{Cmd: "orbit_play", VideoID: "a"})
	send(trans, protocol.BaseMessage{Cmd: "orbit_stop"})
	send(trans, protocol.OrbitRouteMsg{Cmd: "orbit_play", VideoID: "b"})
	close(res.release)
	time.Sleep(50 * time.Millisecond)

	player.mu.Lock()
	defer player.mu.Unlock()
	if player.loadedURL != "url-b" {
		t.Fatalf("expected only b to load, got %q", player.loadedURL)
	}
}

// The end of a track is reported once, as track_ended; Nexus Core advances
// the queue (the satellite must not also ask for "next").
func TestTrackEndedReportsOnlyTheEvent(t *testing.T) {
	_, trans, player := newTestCoordinator(t, resolver.NewDirectResolver())
	send(trans, protocol.OrbitRouteMsg{Cmd: "orbit_play", VideoID: "abc"})
	time.Sleep(50 * time.Millisecond)

	trans.mu.Lock()
	trans.sentMsgs = nil
	trans.mu.Unlock()
	player.mu.Lock()
	ended := player.onTrackEnded
	player.mu.Unlock()
	ended(nil)

	trans.mu.Lock()
	defer trans.mu.Unlock()
	var events, actions int
	for _, m := range trans.sentMsgs {
		var env map[string]any
		_ = json.Unmarshal(m, &env)
		switch env["cmd"] {
		case "orbit_event":
			events++
			if env["event"] != "track_ended" || env["video_id"] != "abc" {
				t.Errorf("unexpected event %v", env)
			}
		case "action":
			actions++
		}
	}
	if events != 1 || actions != 0 {
		t.Errorf("expected one track_ended and no action, got %d events, %d actions", events, actions)
	}
}
