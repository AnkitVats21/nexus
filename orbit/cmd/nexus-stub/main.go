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
	TargetID   string `json:"target_id,omitempty"`
}

type SatelliteSession struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Type         string                  `json:"type"`
	Online       bool                    `json:"online"`
	Capabilities []string                `json:"capabilities"`
	WS           *websocket.Conn         `json:"-"`
	Status       protocol.OrbitReportMsg `json:"status"`
}

type StubServer struct {
	port   int
	wsPath string

	fleetMu      sync.RWMutex
	satellites   map[string]*SatelliteSession
	activeTarget string

	queueMu      sync.Mutex
	queue        []TrackItem
	currentTrack *TrackItem

	uiClientsMu sync.Mutex
	uiClients   map[*websocket.Conn]struct{}

	log *slog.Logger
}

func main() {
	port := flag.Int("port", 8080, "HTTP/WS port for mock Nexus server & Web UI")
	wsPath := flag.String("path", "/api/orbit/ws", "WebSocket endpoint path")
	autoPlay := flag.Bool("autoplay", true, "Automatically play first queued song when satellite connects")
	flag.Parse()

	stub := &StubServer{
		port:         *port,
		wsPath:       *wsPath,
		log:          slog.Default().With("component", "nexus_stub"),
		satellites:   make(map[string]*SatelliteSession),
		uiClients:    make(map[*websocket.Conn]struct{}),
		activeTarget: "nexus-local",
		queue: []TrackItem{
			{VideoID: "dQw4w9WgXcQ", Title: "Never Gonna Give You Up", Artist: "Rick Astley", Duration: 212},
			{VideoID: "kJQP7kiw5Fk", Title: "Despacito", Artist: "Luis Fonsi", Duration: 282},
			{VideoID: "9bZkp7q19f0", Title: "GANGNAM STYLE", Artist: "PSY", Duration: 252},
		},
	}

	// Register simulated default Nexus Core on-board speaker
	stub.satellites["nexus-local"] = &SatelliteSession{
		ID:           "nexus-local",
		Name:         "Nexus Core (ESP32 Speaker)",
		Type:         "speaker",
		Online:       true,
		Capabilities: []string{"audio_playback", "volume_control"},
		Status: protocol.OrbitReportMsg{
			State:  protocol.StateIdle,
			Volume: 80,
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
		stub.log.Info("🌐 Nexus Orbit Constellation Control Center live!",
			"dashboard", fmt.Sprintf("http://localhost:%d", stub.port),
			"satelliteWS", fmt.Sprintf("ws://localhost:%d%s", stub.port, stub.wsPath),
			"initialQueue", len(stub.queue),
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stub.log.Error("Server stopped", "err", err)
			os.Exit(1)
		}
	}()

	if *autoPlay {
		stub.log.Info("Autoplay enabled: active target will route automatically upon connection")
	}

	fmt.Printf("\n✨ Open the Multi-Satellite Web Dashboard: http://localhost:%d\n\n", stub.port)

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

	// Send initial state snapshot
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
	s.log.Info("Web UI Action received", "action", act.Action, "target", act.TargetID, "videoId", act.VideoID)

	switch act.Action {
	case "switch_target":
		s.performOrbitalTransfer(act.TargetID)

	case "play_now":
		title := act.Title
		if title == "" {
			title = fmt.Sprintf("YouTube (%s)", act.VideoID)
		}
		track := TrackItem{VideoID: act.VideoID, Title: title}
		s.queueMu.Lock()
		s.currentTrack = &track
		s.queueMu.Unlock()
		s.routeToActive(track, 0)
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
			s.routeToActive(track, 0)
			s.broadcastStateSnapshot()
		} else {
			s.queueMu.Unlock()
		}

	case "next":
		s.advanceQueue()

	case "prev":
		s.sendCmdToActive(protocol.OrbitSeekMsg{Cmd: "orbit_seek", PositionMs: 0})

	case "pause":
		s.sendCmdToActive(map[string]any{"cmd": "orbit_pause"})

	case "resume":
		s.sendCmdToActive(map[string]any{"cmd": "orbit_resume"})

	case "seek":
		s.sendCmdToActive(protocol.OrbitSeekMsg{Cmd: "orbit_seek", PositionMs: act.PositionMs})

	case "volume":
		s.sendCmdToActive(protocol.OrbitVolumeMsg{Cmd: "orbit_volume", Volume: act.Volume})
	}
}

// ── Multi-Satellite Handoff & Routing ───────────────────────────────────────

func (s *StubServer) performOrbitalTransfer(newTargetID string) {
	s.fleetMu.Lock()
	defer s.fleetMu.Unlock()

	if newTargetID == s.activeTarget {
		return
	}

	newSat, exists := s.satellites[newTargetID]
	if !exists || !newSat.Online {
		s.broadcastLog(fmt.Sprintf("Cannot switch to %s: target offline or unknown", newTargetID))
		return
	}

	oldTargetID := s.activeTarget
	oldSat := s.satellites[oldTargetID]

	var currentPosMs int64 = 0
	if oldSat != nil && oldSat.Online && oldSat.WS != nil {
		currentPosMs = oldSat.Status.PositionMs
		s.log.Info("Pausing playback on old satellite for transfer", "oldTarget", oldTargetID, "posMs", currentPosMs)
		// Pause old satellite
		data, _ := json.Marshal(map[string]any{"cmd": "orbit_pause"})
		_ = oldSat.WS.Write(context.Background(), websocket.MessageText, data)
	}

	s.activeTarget = newTargetID
	s.broadcastLog(fmt.Sprintf("🔄 Orbital Transfer: %s ➡️ %s at %d ms", oldTargetID, newTargetID, currentPosMs))

	// Route current track to new target
	s.queueMu.Lock()
	track := s.currentTrack
	s.queueMu.Unlock()

	if track != nil && newSat.WS != nil {
		s.log.Info("Handoff: routing active track to new satellite", "newTarget", newTargetID, "videoId", track.VideoID, "posMs", currentPosMs)
		msg := protocol.OrbitRouteMsg{
			Cmd:        "orbit_route",
			VideoID:    track.VideoID,
			Title:      track.Title,
			Artist:     track.Artist,
			Duration:   track.Duration,
			PositionMs: currentPosMs,
		}
		data, _ := json.Marshal(msg)
		_ = newSat.WS.Write(context.Background(), websocket.MessageText, data)
	}

	s.broadcastStateSnapshot()
}

func (s *StubServer) routeToActive(track TrackItem, posMs int64) {
	s.fleetMu.RLock()
	activeID := s.activeTarget
	sat := s.satellites[activeID]
	s.fleetMu.RUnlock()

	s.log.Info("Routing track to active target", "target", activeID, "title", track.Title, "videoId", track.VideoID)
	s.broadcastLog(fmt.Sprintf("Routing to [%s]: %s", activeID, track.Title))

	if sat != nil && sat.WS != nil {
		msg := protocol.OrbitRouteMsg{
			Cmd:        "orbit_route",
			VideoID:    track.VideoID,
			Title:      track.Title,
			Artist:     track.Artist,
			Duration:   track.Duration,
			PositionMs: posMs,
		}
		data, _ := json.Marshal(msg)
		_ = sat.WS.Write(context.Background(), websocket.MessageText, data)
	}
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

	s.routeToActive(next, 0)
}

func (s *StubServer) sendCmdToActive(v any) {
	s.fleetMu.RLock()
	sat := s.satellites[s.activeTarget]
	s.fleetMu.RUnlock()

	if sat == nil || sat.WS == nil {
		return
	}

	data, err := json.Marshal(v)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = sat.WS.Write(ctx, websocket.MessageText, data)
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

	var registeredID string

	defer func() {
		if registeredID != "" {
			s.fleetMu.Lock()
			if sat, ok := s.satellites[registeredID]; ok {
				sat.Online = false
				sat.WS = nil
			}
			s.log.Warn("Satellite disconnected from Constellation", "id", registeredID)
			s.broadcastLog(fmt.Sprintf("Satellite offline: %s", registeredID))

			// If active target went offline, failover to another online satellite or nexus-local
			if s.activeTarget == registeredID {
				fallback := "nexus-local"
				for id, sat := range s.satellites {
					if sat.Online && id != registeredID {
						fallback = id
						break
					}
				}
				s.activeTarget = fallback
				s.broadcastLog(fmt.Sprintf("Active satellite disconnected. Failover target: %s", fallback))
			}
			s.fleetMu.Unlock()
			s.broadcastStateSnapshot()
		}
	}()

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}

		var base protocol.BaseMessage
		if err := json.Unmarshal(data, &base); err != nil {
			continue
		}

		switch base.Cmd {
		case "satellite_register":
			var reg protocol.SatelliteRegisterMsg
			if err := json.Unmarshal(data, &reg); err == nil {
				registeredID = reg.Satellite.ID

				s.fleetMu.Lock()
				s.satellites[registeredID] = &SatelliteSession{
					ID:           reg.Satellite.ID,
					Name:         reg.Satellite.Name,
					Type:         reg.Satellite.Type,
					Online:       true,
					Capabilities: reg.Satellite.Capabilities,
					WS:           conn,
					Status: protocol.OrbitReportMsg{
						State:  protocol.StateIdle,
						Volume: 100,
					},
				}

				// If active target is currently nexus-local or unset, adopt this new satellite as active target!
				if s.activeTarget == "nexus-local" || s.activeTarget == "" {
					s.activeTarget = registeredID
				}
				s.fleetMu.Unlock()

				s.log.Info("Satellite joined Constellation", "name", reg.Satellite.Name, "id", registeredID)
				s.broadcastLog(fmt.Sprintf("Satellite online: %s (%s)", reg.Satellite.Name, registeredID))

				// Auto-route first track if idle
				s.queueMu.Lock()
				if len(s.queue) > 0 && s.currentTrack == nil {
					track := s.queue[0]
					s.queue = s.queue[1:]
					s.currentTrack = &track
					s.queueMu.Unlock()
					go s.routeToActive(track, 0)
				} else {
					s.queueMu.Unlock()
				}

				s.broadcastStateSnapshot()
			}

		case "orbit_report":
			var rep protocol.OrbitReportMsg
			if err := json.Unmarshal(data, &rep); err == nil {
				s.fleetMu.Lock()
				if sat, ok := s.satellites[registeredID]; ok {
					sat.Status = rep
				}
				isActive := (registeredID == s.activeTarget)
				s.fleetMu.Unlock()

				// If report is from active satellite, broadcast telemetry to UI
				if isActive {
					s.broadcastJSON(map[string]any{
						"type":        "telemetry",
						"state":       rep.State,
						"position_ms": rep.PositionMs,
						"duration_ms": rep.DurationMs,
						"volume":      rep.Volume,
						"video_id":    rep.VideoID,
					})
				}
			}

		case "orbit_event":
			var evt protocol.OrbitEventMsg
			if err := json.Unmarshal(data, &evt); err == nil {
				s.broadcastLog(fmt.Sprintf("[%s] Event: %s (videoId: %s)", registeredID, evt.Event, evt.VideoID))
			}

		case "action":
			var act protocol.NexusActionMsg
			if err := json.Unmarshal(data, &act); err == nil {
				s.broadcastLog(fmt.Sprintf("[%s] Media Key: %s", registeredID, act.Action))
				if act.Action == "next" {
					s.advanceQueue()
				} else if act.Action == "toggle" {
					s.sendCmdToActive(map[string]any{"cmd": "orbit_pause"})
				}
			}
		}
	}
}

// ── UI Snapshot & Broadcast ─────────────────────────────────────────────────

func (s *StubServer) getConstellationList() ([]*SatelliteSession, string) {
	s.fleetMu.RLock()
	defer s.fleetMu.RUnlock()

	list := make([]*SatelliteSession, 0, len(s.satellites))
	for _, sat := range s.satellites {
		cp := *sat
		list = append(list, &cp)
	}
	return list, s.activeTarget
}

func (s *StubServer) getActivePlayerStatus() protocol.OrbitReportMsg {
	s.fleetMu.RLock()
	defer s.fleetMu.RUnlock()
	if sat, ok := s.satellites[s.activeTarget]; ok {
		return sat.Status
	}
	return protocol.OrbitReportMsg{State: protocol.StateIdle, Volume: 100}
}

func (s *StubServer) sendStateSnapshot(conn *websocket.Conn) {
	constellation, activeTarget := s.getConstellationList()
	status := s.getActivePlayerStatus()

	s.queueMu.Lock()
	cur := s.currentTrack
	q := make([]TrackItem, len(s.queue))
	copy(q, s.queue)
	s.queueMu.Unlock()

	payload := map[string]any{
		"type":          "state_snapshot",
		"constellation": constellation,
		"active_target": activeTarget,
		"current_track": cur,
		"player_status": status,
		"queue":         q,
	}

	data, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, data)
}

func (s *StubServer) broadcastStateSnapshot() {
	constellation, activeTarget := s.getConstellationList()
	status := s.getActivePlayerStatus()

	s.queueMu.Lock()
	cur := s.currentTrack
	q := make([]TrackItem, len(s.queue))
	copy(q, s.queue)
	s.queueMu.Unlock()

	s.broadcastJSON(map[string]any{
		"type":          "state_snapshot",
		"constellation": constellation,
		"active_target": activeTarget,
		"current_track": cur,
		"player_status": status,
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
