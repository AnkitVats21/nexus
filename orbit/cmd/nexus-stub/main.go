package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"nexus-orbit/pkg/protocol"
)

//go:embed index.html
var indexHTML []byte

type TrackItem struct {
	VideoID  string `json:"videoId"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"`
}

type UIActionMsg struct {
	Action     string `json:"action"`
	VideoID    string `json:"videoId,omitempty"`
	Title      string `json:"title,omitempty"`
	Index      int    `json:"index,omitempty"`
	PositionMs int64  `json:"position_ms,omitempty"`
	Volume     int    `json:"volume,omitempty"`
}

type StubServer struct {
	port      int
	wsPath    string
	connMu    sync.Mutex
	activeWS  *websocket.Conn
	satellite *protocol.SatelliteInfo

	queueMu      sync.Mutex
	queue        []TrackItem
	currentTrack *TrackItem
	playerStatus protocol.OrbitReportMsg

	uiClientsMu sync.Mutex
	uiClients   map[*websocket.Conn]struct{}

	log *slog.Logger
}

func main() {
	port := flag.Int("port", 8080, "HTTP/WS port for the mock Nexus server & Web UI")
	wsPath := flag.String("path", "/api/orbit/ws", "WebSocket endpoint path")
	autoPlay := flag.Bool("autoplay", true, "Automatically play first queued song when satellite connects")
	flag.Parse()

	stub := &StubServer{
		port:      *port,
		wsPath:    *wsPath,
		log:       slog.Default().With("component", "nexus_stub"),
		uiClients: make(map[*websocket.Conn]struct{}),
		queue: []TrackItem{
			{VideoID: "dQw4w9WgXcQ", Title: "Never Gonna Give You Up", Artist: "Rick Astley", Duration: 212},
			{VideoID: "kJQP7kiw5Fk", Title: "Despacito", Artist: "Luis Fonsi", Duration: 282},
			{VideoID: "9bZkp7q19f0", Title: "GANGNAM STYLE", Artist: "PSY", Duration: 252},
		},
		playerStatus: protocol.OrbitReportMsg{
			State:  protocol.StateIdle,
			Volume: 100,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", stub.handleWebUI)
	mux.HandleFunc("/api/stub/ui-ws", stub.handleUIWebSocket)
	mux.HandleFunc(stub.wsPath, stub.handleSatelliteWS)
	mux.HandleFunc("/api/ws", stub.handleSatelliteWS)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", stub.port),
		Handler: mux,
	}

	go func() {
		stub.log.Info("🌐 Nexus Orbit Web Dashboard & Stub Server live!",
			"url", fmt.Sprintf("http://localhost:%d", stub.port),
			"satelliteWS", fmt.Sprintf("ws://localhost:%d%s", stub.port, stub.wsPath),
			"queuedTracks", len(stub.queue),
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stub.log.Error("Server stopped", "err", err)
			os.Exit(1)
		}
	}()

	if *autoPlay {
		stub.log.Info("Autoplay enabled: first track will route automatically upon satellite connection")
	}

	fmt.Printf("\n✨ Open the Web Dashboard in your browser: http://localhost:%d\n\n", stub.port)

	// Keep alive
	select {}
}

// ── Web UI Handlers ─────────────────────────────────────────────────────────

func (s *StubServer) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *StubServer) handleUIWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	s.uiClientsMu.Lock()
	s.uiClients[conn] = struct{}{}
	s.uiClientsMu.Unlock()

	defer func() {
		s.uiClientsMu.Lock()
		delete(s.uiClients, conn)
		s.uiClientsMu.Unlock()
	}()

	// Send initial snapshot
	s.sendStateSnapshot(conn)

	// Read actions from Web UI
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}

		var act UIActionMsg
		if err := json.Unmarshal(data, &act); err == nil {
			s.handleUIAction(act)
		}
	}
}

func (s *StubServer) handleUIAction(act UIActionMsg) {
	s.log.Info("Web UI Action received", "action", act.Action, "videoId", act.VideoID)

	switch act.Action {
	case "play_now":
		title := act.Title
		if title == "" {
			title = fmt.Sprintf("YouTube (%s)", act.VideoID)
		}
		track := TrackItem{VideoID: act.VideoID, Title: title}
		s.queueMu.Lock()
		s.currentTrack = &track
		s.queueMu.Unlock()
		s.sendRoute(track, 0)
		s.broadcastStateSnapshot()

	case "queue_add":
		title := act.Title
		if title == "" {
			title = fmt.Sprintf("YouTube (%s)", act.VideoID)
		}
		s.queueMu.Lock()
		s.queue = append(s.queue, TrackItem{VideoID: act.VideoID, Title: title})
		s.queueMu.Unlock()
		s.broadcastStateSnapshot()

	case "queue_remove":
		s.queueMu.Lock()
		if act.Index >= 0 && act.Index < len(s.queue) {
			s.queue = append(s.queue[:act.Index], s.queue[act.Index+1:]...)
		}
		s.queueMu.Unlock()
		s.broadcastStateSnapshot()

	case "play_queue_index":
		s.queueMu.Lock()
		if act.Index >= 0 && act.Index < len(s.queue) {
			track := s.queue[act.Index]
			s.queue = append(s.queue[:act.Index], s.queue[act.Index+1:]...)
			s.currentTrack = &track
			s.queueMu.Unlock()
			s.sendRoute(track, 0)
			s.broadcastStateSnapshot()
		} else {
			s.queueMu.Unlock()
		}

	case "next":
		s.advanceQueue()

	case "prev":
		// Seek to start or previous
		s.sendSatelliteCmd(protocol.OrbitSeekMsg{Cmd: "orbit_seek", PositionMs: 0})

	case "pause":
		s.sendSatelliteCmd(map[string]any{"cmd": "orbit_pause"})

	case "resume":
		s.sendSatelliteCmd(map[string]any{"cmd": "orbit_resume"})

	case "seek":
		s.sendSatelliteCmd(protocol.OrbitSeekMsg{Cmd: "orbit_seek", PositionMs: act.PositionMs})

	case "volume":
		s.sendSatelliteCmd(protocol.OrbitVolumeMsg{Cmd: "orbit_volume", Volume: act.Volume})
	}
}

// ── Satellite WebSocket Handler ─────────────────────────────────────────────

func (s *StubServer) handleSatelliteWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Error("Satellite WS accept error", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	s.connMu.Lock()
	s.activeWS = conn
	s.connMu.Unlock()

	s.log.Info("Satellite connected", "remote", r.RemoteAddr)

	defer func() {
		s.connMu.Lock()
		if s.activeWS == conn {
			s.activeWS = nil
			s.satellite = nil
		}
		s.connMu.Unlock()
		s.log.Warn("Satellite disconnected")
		s.broadcastStateSnapshot()
	}()

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}

		s.handleSatelliteMessage(data)
	}
}

func (s *StubServer) handleSatelliteMessage(data []byte) {
	var base protocol.BaseMessage
	if err := json.Unmarshal(data, &base); err != nil {
		return
	}

	switch base.Cmd {
	case "satellite_register":
		var reg protocol.SatelliteRegisterMsg
		if err := json.Unmarshal(data, &reg); err == nil {
			s.connMu.Lock()
			s.satellite = &reg.Satellite
			s.connMu.Unlock()

			s.log.Info("Satellite registered", "name", reg.Satellite.Name, "id", reg.Satellite.ID)
			s.broadcastLog(fmt.Sprintf("Satellite registered: %s (%s)", reg.Satellite.Name, reg.Satellite.ID))

			// Auto-route first track if available
			s.queueMu.Lock()
			if len(s.queue) > 0 && s.currentTrack == nil {
				track := s.queue[0]
				s.queue = s.queue[1:]
				s.currentTrack = &track
				s.queueMu.Unlock()
				go s.sendRoute(track, 0)
			} else {
				s.queueMu.Unlock()
			}

			s.broadcastStateSnapshot()
		}

	case "orbit_report":
		var rep protocol.OrbitReportMsg
		if err := json.Unmarshal(data, &rep); err == nil {
			s.queueMu.Lock()
			s.playerStatus = rep
			s.queueMu.Unlock()

			s.broadcastJSON(map[string]any{
				"type":        "telemetry",
				"state":       rep.State,
				"position_ms": rep.PositionMs,
				"duration_ms": rep.DurationMs,
				"volume":      rep.Volume,
				"video_id":    rep.VideoID,
			})
		}

	case "orbit_event":
		var evt protocol.OrbitEventMsg
		if err := json.Unmarshal(data, &evt); err == nil {
			s.broadcastLog(fmt.Sprintf("Event: %s (videoId: %s)", evt.Event, evt.VideoID))
		}

	case "action":
		var act protocol.NexusActionMsg
		if err := json.Unmarshal(data, &act); err == nil {
			s.broadcastLog(fmt.Sprintf("Media Key Action: %s", act.Action))
			if act.Action == "next" {
				s.advanceQueue()
			} else if act.Action == "toggle" {
				s.sendSatelliteCmd(map[string]any{"cmd": "orbit_pause"})
			}
		}
	}
}

func (s *StubServer) sendRoute(track TrackItem, posMs int64) {
	s.log.Info("Routing track to Satellite", "title", track.Title, "videoId", track.VideoID)
	s.broadcastLog(fmt.Sprintf("Routing track: %s (%s)", track.Title, track.VideoID))

	msg := protocol.OrbitRouteMsg{
		Cmd:        "orbit_route",
		VideoID:    track.VideoID,
		Title:      track.Title,
		Artist:     track.Artist,
		Duration:   track.Duration,
		PositionMs: posMs,
	}
	s.sendSatelliteCmd(msg)
	s.broadcastStateSnapshot()
}

func (s *StubServer) advanceQueue() {
	s.queueMu.Lock()
	if len(s.queue) == 0 {
		s.queueMu.Unlock()
		s.broadcastLog("Queue reached end.")
		return
	}
	next := s.queue[0]
	s.queue = s.queue[1:]
	s.currentTrack = &next
	s.queueMu.Unlock()

	s.sendRoute(next, 0)
}

func (s *StubServer) sendSatelliteCmd(v any) {
	s.connMu.Lock()
	conn := s.activeWS
	s.connMu.Unlock()

	if conn == nil {
		return
	}

	data, err := json.Marshal(v)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

// ── UI Broadcast Helpers ───────────────────────────────────────────────────

func (s *StubServer) sendStateSnapshot(conn *websocket.Conn) {
	s.connMu.Lock()
	sat := s.satellite
	s.connMu.Unlock()

	s.queueMu.Lock()
	cur := s.currentTrack
	q := make([]TrackItem, len(s.queue))
	copy(q, s.queue)
	st := s.playerStatus
	s.queueMu.Unlock()

	payload := map[string]any{
		"type":          "state_snapshot",
		"satellite":     sat,
		"current_track": cur,
		"player_status": st,
		"queue":         q,
	}

	data, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

func (s *StubServer) broadcastStateSnapshot() {
	s.connMu.Lock()
	sat := s.satellite
	s.connMu.Unlock()

	s.queueMu.Lock()
	cur := s.currentTrack
	q := make([]TrackItem, len(s.queue))
	copy(q, s.queue)
	st := s.playerStatus
	s.queueMu.Unlock()

	s.broadcastJSON(map[string]any{
		"type":          "state_snapshot",
		"satellite":     sat,
		"current_track": cur,
		"player_status": st,
		"queue":         q,
	})
}

func (s *StubServer) broadcastLog(text string) {
	s.broadcastJSON(map[string]any{
		"type": "log",
		"text": text,
	})
}

func (s *StubServer) broadcastJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}

	s.uiClientsMu.Lock()
	defer s.uiClientsMu.Unlock()

	for conn := range s.uiClients {
		go func(c *websocket.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = c.Write(ctx, websocket.MessageText, data)
		}(conn)
	}
}
