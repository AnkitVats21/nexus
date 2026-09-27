package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"nexus-orbit/pkg/protocol"
)

type TrackItem struct {
	VideoID  string `json:"videoId"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"`
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

	log *slog.Logger
}

func main() {
	port := flag.Int("port", 8080, "HTTP/WS port for the mock Nexus server")
	wsPath := flag.String("path", "/api/orbit/ws", "WebSocket endpoint path")
	autoPlay := flag.Bool("autoplay", true, "Automatically play first queued song when satellite connects")
	flag.Parse()

	stub := &StubServer{
		port:   *port,
		wsPath: *wsPath,
		log:    slog.Default().With("component", "nexus_stub"),
		queue: []TrackItem{
			{VideoID: "dQw4w9WgXcQ", Title: "Never Gonna Give You Up", Artist: "Rick Astley", Duration: 212},
			{VideoID: "kJQP7kiw5Fk", Title: "Despacito", Artist: "Luis Fonsi", Duration: 282},
			{VideoID: "9bZkp7q19f0", Title: "GANGNAM STYLE", Artist: "PSY", Duration: 252},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(stub.wsPath, stub.handleWS)
	mux.HandleFunc("/api/ws", stub.handleWS) // also listen on standard /api/ws

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", stub.port),
		Handler: mux,
	}

	go func() {
		stub.log.Info("📡 Nexus Core Mock Stub running",
			"addr", fmt.Sprintf("http://localhost:%d%s", stub.port, stub.wsPath),
			"queuedTracks", len(stub.queue),
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			stub.log.Error("Stub server failed", "err", err)
			os.Exit(1)
		}
	}()

	// If autoplay is enabled, auto-route the first track when satellite connects
	if *autoPlay {
		stub.log.Info("Autoplay enabled: first track will be routed upon satellite registration")
	}

	stub.printHelp()
	stub.cliLoop()
}

func (s *StubServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Error("WebSocket accept error", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	s.connMu.Lock()
	s.activeWS = conn
	s.connMu.Unlock()

	s.log.Info("Satellite connected to mock Nexus Core", "remote", r.RemoteAddr)

	defer func() {
		s.connMu.Lock()
		if s.activeWS == conn {
			s.activeWS = nil
			s.satellite = nil
		}
		s.connMu.Unlock()
		s.log.Warn("Satellite disconnected from mock Nexus Core")
	}()

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}

		s.handleInboundSatelliteMessage(data)
	}
}

func (s *StubServer) handleInboundSatelliteMessage(data []byte) {
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
			fmt.Printf("\n✨ [REGISTRATION] Satellite registered: %s (ID: %s, Type: %s)\n> ",
				reg.Satellite.Name, reg.Satellite.ID, reg.Satellite.Type)

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
		}

	case "orbit_report":
		var rep protocol.OrbitReportMsg
		if err := json.Unmarshal(data, &rep); err == nil {
			posSec := rep.PositionMs / 1000
			durSec := rep.DurationMs / 1000
			// Print compact telemetry
			fmt.Printf("\r📊 [%s] %s | %02d:%02d / %02d:%02d | Vol: %d%%   ",
				rep.State, rep.VideoID, posSec/60, posSec%60, durSec/60, durSec%60, rep.Volume)
		}

	case "orbit_event":
		var evt protocol.OrbitEventMsg
		if err := json.Unmarshal(data, &evt); err == nil {
			fmt.Printf("\n🔔 [EVENT] %s (videoId: %s, msg: %s)\n> ", evt.Event, evt.VideoID, evt.Message)
		}

	case "action":
		var act protocol.NexusActionMsg
		if err := json.Unmarshal(data, &act); err == nil {
			fmt.Printf("\n🎮 [USER ACTION from Satellite] %s (value: %v)\n> ", act.Action, act.Value)
			if act.Action == "next" {
				s.advanceQueue()
			} else if act.Action == "toggle" {
				// Toggle
				s.sendCmd(map[string]any{"cmd": "orbit_pause"})
			}
		}
	}
}

func (s *StubServer) sendRoute(track TrackItem, posMs int64) {
	fmt.Printf("\n🚀 [NEXUS -> ORBIT] Routing track: %s (%s) at %d ms\n> ", track.Title, track.VideoID, posMs)
	msg := protocol.OrbitRouteMsg{
		Cmd:        "orbit_route",
		VideoID:    track.VideoID,
		Title:      track.Title,
		Artist:     track.Artist,
		Duration:   track.Duration,
		PositionMs: posMs,
	}
	s.sendCmd(msg)
}

func (s *StubServer) advanceQueue() {
	s.queueMu.Lock()
	if len(s.queue) == 0 {
		s.queueMu.Unlock()
		fmt.Printf("\n📭 [QUEUE EMPTY] No more tracks in mock queue.\n> ")
		return
	}
	next := s.queue[0]
	s.queue = s.queue[1:]
	s.currentTrack = &next
	s.queueMu.Unlock()

	s.sendRoute(next, 0)
}

func (s *StubServer) sendCmd(v any) {
	s.connMu.Lock()
	conn := s.activeWS
	s.connMu.Unlock()

	if conn == nil {
		fmt.Println("⚠️ Cannot send command: no satellite connected!")
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

func (s *StubServer) printHelp() {
	fmt.Println(`
Commands:
  play <videoId> [title] - Immediately route and play videoId
  queue <videoId> [title]- Add a track to the end of the queue
  next                   - Pop and route the next track in queue
  pause                  - Pause active playback
  resume                 - Resume playback
  seek <ms>              - Seek to position in milliseconds
  vol <0-100>            - Set satellite volume
  list                   - List tracks remaining in queue
  help                   - Show this help
  quit                   - Exit stub server
`)
}

func (s *StubServer) cliLoop() {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "play":
			if len(parts) < 2 {
				fmt.Println("Usage: play <videoId> [title]")
				continue
			}
			title := "Direct Play"
			if len(parts) >= 3 {
				title = strings.Join(parts[2:], " ")
			}
			track := TrackItem{VideoID: parts[1], Title: title}
			s.queueMu.Lock()
			s.currentTrack = &track
			s.queueMu.Unlock()
			s.sendRoute(track, 0)

		case "queue":
			if len(parts) < 2 {
				fmt.Println("Usage: queue <videoId> [title]")
				continue
			}
			title := "Queued Track"
			if len(parts) >= 3 {
				title = strings.Join(parts[2:], " ")
			}
			s.queueMu.Lock()
			s.queue = append(s.queue, TrackItem{VideoID: parts[1], Title: title})
			qLen := len(s.queue)
			s.queueMu.Unlock()
			fmt.Printf("Added to queue. Queue length: %d\n", qLen)

		case "next":
			s.advanceQueue()

		case "pause":
			s.sendCmd(map[string]any{"cmd": "orbit_pause"})

		case "resume":
			s.sendCmd(map[string]any{"cmd": "orbit_resume"})

		case "seek":
			if len(parts) < 2 {
				fmt.Println("Usage: seek <position_ms>")
				continue
			}
			ms, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				fmt.Println("Invalid ms value")
				continue
			}
			s.sendCmd(protocol.OrbitSeekMsg{Cmd: "orbit_seek", PositionMs: ms})

		case "vol", "volume":
			if len(parts) < 2 {
				fmt.Println("Usage: vol <0-100>")
				continue
			}
			vol, err := strconv.Atoi(parts[1])
			if err != nil {
				fmt.Println("Invalid volume value")
				continue
			}
			s.sendCmd(protocol.OrbitVolumeMsg{Cmd: "orbit_volume", Volume: vol})

		case "list":
			s.queueMu.Lock()
			fmt.Printf("Upcoming Queue (%d tracks):\n", len(s.queue))
			for i, t := range s.queue {
				fmt.Printf("  %d. %s (%s)\n", i+1, t.Title, t.VideoID)
			}
			s.queueMu.Unlock()

		case "help":
			s.printHelp()

		case "quit", "exit":
			fmt.Println("Exiting...")
			os.Exit(0)

		default:
			fmt.Printf("Unknown command %q. Type 'help' for options.\n", cmd)
		}
	}
}
