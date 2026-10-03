// Package hub is nexus-hub, the brain on the LAN: devices (edges: the
// waveshare board, Orbit, later others) connect to it with the roles they
// take, and it serves the dashboard to any number of browsers. The protocol
// is docs/PROTOCOL.md; the plan is docs/plans/hub-relay-design.md in the
// waveshare repository.
//
//   - /hub/edge (and /hub/board): an edge's WebSocket. hello (token, roles)
//     -> welcome. Voice sessions (voice.go), music (music.go), and for the
//     edge with the state role (the board) its state pushes (JSON without
//     "type", the same as the board's /api/ws) and transcript messages,
//     with browser commands going down.
//   - /api/ws: browsers. Each gets the latest state at once and every push
//     after; their {"cmd"} frames go to the board. {"cmd":"subscribe"} is
//     handled here (the hub keeps the transcript).
//   - /api/*: forwarded to the board's own HTTP server.
//   - everything else: the dashboard build (WebDir).
package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	ProtocolVersion = 2
	EdgePath        = "/hub/edge"
	BoardPath       = "/hub/board" // proto 1 boards; the same endpoint

	helloTimeout    = 10 * time.Second
	pingInterval    = 5 * time.Second
	pingTimeout     = 10 * time.Second
	writeTimeout    = 5 * time.Second
	browserQueueLen = 64
	boardQueueLen   = 64
	maxBoardMessage = 256 << 10
	maxBrowserFrame = 4 << 10

	// Close codes.
	closeRejected     = websocket.StatusCode(4001)
	closeReplaced     = websocket.StatusCode(4002)
	closeBoardOffline = websocket.StatusCode(4003)
)

type Config struct {
	Version string
	// Board tokens accepted in the hello. Empty: every board is refused.
	Tokens []string
	// The dashboard build to serve ("" = none).
	WebDir string
	Log    *slog.Logger

	// Voice: the Gemini API key ("" = no voice through the hub) and the Live
	// endpoint (DefaultGeminiURL; tests use a fake).
	GeminiKey string
	GeminiURL string
	// Voice uplink format: auto|pcm|opus (default "auto")
	Uplink string
	// Tools for voice sessions: the local ones, the remote MCP server's, or
	// both (ToolSet); nil = none.
	MCP Tools
	// How far ahead of playback the reply is sent (defaultVoiceLead).
	VoiceLead time.Duration
	// Where songs are looked up (nil = the hub doesn't run the music).
	Tracks TrackSource
	// Songs kept for the players (nil = none; they stream from YouTube).
	Cache *MusicCache
}

type Hub struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	edges     map[string]*edge // by id
	board     *edge            // the edge with the state role (the dashboard's)
	lastState []byte
	browsers  map[*browser]struct{}

	music *Music

	transcript *Transcript
	voices     map[string]*voiceSession // by board id
}

type edge struct {
	conn    *websocket.Conn
	send    chan []byte
	cancel  context.CancelFunc
	id      string
	name    string
	kind    string
	fw      string
	roles   edgeRoles
	legacy  bool     // proto 1 hello (no roles): runs its own music
	voiceUp string   // negotiated uplink: "opus" or "pcm"
	apiBase *url.URL // state role only
	base    string   // the hub as this edge reached it: "http://host:port"
	proxy   *httputil.ReverseProxy
	since   time.Time
	voiceQ  *voiceQueue
	voice   *voiceSession
}

// edgeRoles are the roles an edge takes (docs/PROTOCOL.md section 2); nil
// is a role not taken.
type edgeRoles struct {
	Voice *struct {
		Up   []string `json:"up"`
		Down []string `json:"down"`
	} `json:"voice,omitempty"`
	Player     *playerRole `json:"player,omitempty"`
	Controller bool        `json:"controller,omitempty"`
	State      *struct {
		HTTPPort int `json:"http_port"`
	} `json:"state,omitempty"`
}

type playerRole struct {
	URL       bool `json:"url"`        // plays a stream URL
	Resolves  bool `json:"resolves"`   // plays from a video id alone
	QueueCopy bool `json:"queue_copy"` // keeps the queue, carries on alone
}

type browser struct {
	conn       *websocket.Conn
	send       chan []byte
	cancel     context.CancelFunc
	transcript bool // subscribed; guarded by Hub.mu
}

type helloMsg struct {
	Type     string     `json:"type"`
	Proto    int        `json:"proto"`
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Kind     string     `json:"kind"`
	FW       string     `json:"fw"`
	HTTPPort int        `json:"http_port"` // proto 1
	Token    string     `json:"token"`
	Roles    *edgeRoles `json:"roles"`
}

func New(cfg Config) *Hub {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.GeminiURL == "" {
		cfg.GeminiURL = DefaultGeminiURL
	}
	if m, ok := cfg.MCP.(*MCPClient); cfg.MCP == nil || (ok && m == nil) {
		cfg.MCP = noTools{}
	}
	if cfg.VoiceLead <= 0 {
		cfg.VoiceLead = defaultVoiceLead
	}
	h := &Hub{
		cfg:        cfg,
		log:        cfg.Log.With("component", "hub"),
		edges:      map[string]*edge{},
		browsers:   map[*browser]struct{}{},
		transcript: NewTranscript(),
		voices:     map[string]*voiceSession{},
	}
	if cfg.Tracks != nil {
		h.music = newMusic(h, cfg.Tracks)
	}
	return h
}

// Handler serves the board endpoint, the browser socket, the API proxy and
// the dashboard.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(EdgePath, h.serveEdge)
	mux.HandleFunc(BoardPath, h.serveEdge)
	mux.HandleFunc("/hub/status", h.serveStatus)
	if h.cfg.Cache != nil {
		mux.Handle(CachePath, h.cfg.Cache)
	}
	mux.HandleFunc("/api/ws", h.serveBrowser)
	mux.HandleFunc("/api/", h.serveAPI)
	mux.HandleFunc("/", h.serveWeb)
	return mux
}

// ─────────────────────────────────────────────────────────────────────────────
// Edges
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) tokenOK(token string) bool {
	ok := false
	for _, t := range h.cfg.Tokens {
		if t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

func (h *Hub) serveEdge(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxBoardMessage)

	helloCtx, cancelHello := context.WithTimeout(r.Context(), helloTimeout)
	typ, data, err := conn.Read(helloCtx)
	cancelHello()
	var hello helloMsg
	if err != nil || typ != websocket.MessageText || json.Unmarshal(data, &hello) != nil || hello.Type != "hello" || hello.ID == "" {
		conn.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	if !h.tokenOK(hello.Token) {
		h.log.Warn("Edge refused: bad token", "id", hello.ID, "addr", r.RemoteAddr)
		h.writeNow(conn, map[string]any{"type": "rejected", "reason": "bad token"})
		conn.Close(closeRejected, "bad token")
		return
	}

	roles := hello.Roles
	if roles == nil {
		// Proto 1: the waveshare board, voice and state.
		roles = &edgeRoles{}
		roles.Voice = &struct {
			Up   []string `json:"up"`
			Down []string `json:"down"`
		}{}
		roles.State = &struct {
			HTTPPort int `json:"http_port"`
		}{HTTPPort: hello.HTTPPort}
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &edge{
		conn: conn, send: make(chan []byte, boardQueueLen), cancel: cancel,
		id: hello.ID, name: hello.Name, kind: hello.Kind, fw: hello.FW, roles: *roles, legacy: hello.Roles == nil,
		since: time.Now(), voiceQ: newVoiceQueue(conn, h.cfg.VoiceLead),
		base: "http://" + r.Host,
	}
	if b.name == "" {
		b.name = b.id
	}
	if roles.State != nil {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		port := roles.State.HTTPPort
		if port == 0 {
			port = 80
		}
		b.apiBase = &url.URL{Scheme: "http", Host: net.JoinHostPort(host, fmt.Sprint(port))}
		b.proxy = newProxy(b.apiBase)
	}
	b.voice = h.voiceFor(b.id)

	h.mu.Lock()
	old := h.edges[b.id]
	h.edges[b.id] = b
	if roles.State != nil {
		h.board = b
		h.lastState = nil
	}
	h.mu.Unlock()
	if roles.State != nil {
		h.transcript.Reset()
	}
	if old != nil {
		old.conn.Close(closeReplaced, "another connection with this id")
		old.cancel()
	}

	b.voiceUp = h.negotiateUplink(roles)

	// voice: PCM or Opus, or false without a Gemini key (the board then
	// keeps its sessions direct).
	var voice any = false
	if h.voiceEnabled() {
		voice = map[string]any{"up": b.voiceUp, "down": "pcm", "down_rate": voiceRate}
	}
	proto := min(ProtocolVersion, max(hello.Proto, 1))
	welcome, _ := json.Marshal(map[string]any{"type": "welcome", "hub": "nexus-hub " + h.cfg.Version,
		"proto": proto, "voice": voice, "music": h.music != nil})
	if err := h.writeNow(conn, welcome); err != nil {
		h.dropBoard(b, err)
		return
	}
	h.log.Info("Edge connected", "id", b.id, "name", b.name, "kind", b.kind, "fw", b.fw, "roles", b.roles.String(), "uplink", b.voiceUp)

	b.voice.attach(b)
	if h.music != nil {
		h.music.edgeJoined(b)
	}
	go h.boardWriter(ctx, b)
	go b.voiceQ.run(ctx, func(err error) { h.dropBoard(b, err) })
	go h.pinger(ctx, conn, func(err error) { h.dropBoard(b, err) })
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			h.dropBoard(b, err)
			return
		}
		t0 := time.Now()
		switch {
		case typ == websocket.MessageText:
			h.fromBoard(b, data)
		case len(data) >= 2 && data[0] == chanMic:
			b.voice.mic(data[1], data[2:])
		}
		if d := time.Since(t0); d > 300*time.Millisecond {
			h.log.Warn("Board message took long to handle", "ms", d.Milliseconds(), "binary", typ == websocket.MessageBinary)
		}
	}
}

func (h *Hub) negotiateUplink(roles *edgeRoles) string {
	var offers []string
	if roles != nil && roles.Voice != nil && len(roles.Voice.Up) > 0 {
		offers = roles.Voice.Up
	} else {
		offers = []string{"pcm16"}
	}
	hasOpus16 := false
	for _, o := range offers {
		if strings.EqualFold(strings.TrimSpace(o), "opus16") {
			hasOpus16 = true
			break
		}
	}
	mode := strings.ToLower(strings.TrimSpace(h.cfg.Uplink))
	switch mode {
	case "pcm":
		return "pcm"
	case "opus", "auto", "":
		if hasOpus16 {
			return "opus"
		}
		return "pcm"
	default:
		if hasOpus16 {
			return "opus"
		}
		return "pcm"
	}
}

func (h *Hub) fromBoard(b *edge, data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		h.log.Warn("Unreadable board message", "bytes", len(data), "start", fmt.Sprintf("%q", data[:min(len(data), 48)]))
		return
	}
	switch head.Type {
	case "":
		// A state push: the latest goes to every browser, and to new ones.
		h.mu.Lock()
		current := h.board == b
		if current {
			h.lastState = data
		}
		h.mu.Unlock()
		if current {
			h.broadcast(data, false)
		}
	case "transcript":
		if err := h.transcript.Merge(data); err != nil {
			h.log.Warn("Bad transcript message", "err", err)
			return
		}
		h.broadcast(data, true)
	case "session_start", "text_turn", "tool_response", "interrupt", "session_end":
		b.voice.fromBoard(head.Type, data)
	case "voice_activity", "music_action", "music_report", "music_event", "music_renew", "music_sync":
		if h.music != nil {
			h.music.fromEdge(b, head.Type, data)
		}
	default:
		h.log.Debug("Ignoring board message", "type", head.Type)
	}
}

func (h *Hub) boardWriter(ctx context.Context, b *edge) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-b.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := b.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				h.dropBoard(b, err)
				return
			}
		}
	}
}

func (h *Hub) dropBoard(b *edge, err error) {
	h.mu.Lock()
	present := h.edges[b.id] == b
	if present {
		delete(h.edges, b.id)
	}
	current := h.board == b
	var browsers []*browser
	if current {
		h.board = nil
		h.lastState = nil
		for br := range h.browsers {
			browsers = append(browsers, br)
		}
	}
	h.mu.Unlock()
	b.cancel()
	b.conn.Close(websocket.StatusGoingAway, "")
	b.voice.detach(b)
	if !present {
		return
	}
	if h.music != nil {
		h.music.edgeLeft(b)
	}
	h.log.Info("Edge disconnected", "id", b.id, "reason", errText(err))
	if !current {
		return
	}
	// The browsers reconnect and wait for the board. Each close waits for
	// the browser's answer (up to 5 s), so one stuck browser mustn't hold
	// up the others.
	for _, br := range browsers {
		go func() {
			br.conn.Close(closeBoardOffline, "board offline")
			br.cancel()
		}()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Browsers
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) serveBrowser(w http.ResponseWriter, r *http.Request) {
	// Same rule as the board's own /api/ws: any origin (dev builds run elsewhere).
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxBrowserFrame)

	h.mu.Lock()
	b := h.board
	if b == nil {
		h.mu.Unlock()
		conn.Close(closeBoardOffline, "board offline")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	br := &browser{conn: conn, send: make(chan []byte, browserQueueLen), cancel: cancel}
	h.browsers[br] = struct{}{}
	if h.lastState != nil {
		br.send <- h.lastState
	}
	n := len(h.browsers)
	h.mu.Unlock()
	h.log.Info("Browser connected", "addr", r.RemoteAddr, "browsers", n)

	defer h.dropBrowser(br)
	go h.browserWriter(ctx, br)
	go h.pinger(ctx, conn, func(error) { h.dropBrowser(br) })
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			h.fromBrowser(br, data)
		}
	}
}

func (h *Hub) fromBrowser(br *browser, data []byte) {
	var cmd struct {
		Cmd        string `json:"cmd"`
		Transcript bool   `json:"transcript"`
	}
	if err := json.Unmarshal(data, &cmd); err != nil || cmd.Cmd == "" {
		return
	}
	if cmd.Cmd == "subscribe" {
		h.mu.Lock()
		br.transcript = cmd.Transcript
		h.mu.Unlock()
		if full := h.transcript.Full(); cmd.Transcript && full != nil {
			h.queueBrowser(br, full)
		}
		return
	}
	h.mu.Lock()
	b := h.board
	h.mu.Unlock()
	if b == nil {
		return
	}
	select {
	case b.send <- data:
	default:
		h.log.Warn("Board send queue full; dropped a command", "cmd", cmd.Cmd)
	}
}

func (h *Hub) browserWriter(ctx context.Context, br *browser) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-br.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := br.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				h.dropBrowser(br)
				return
			}
		}
	}
}

func (h *Hub) queueBrowser(br *browser, msg []byte) {
	select {
	case br.send <- msg:
	default:
		// Too slow to keep up: drop it; the dashboard reconnects.
		h.log.Warn("Browser too slow; dropping it")
		go h.dropBrowser(br)
	}
}

func (h *Hub) broadcast(msg []byte, transcriptOnly bool) {
	h.mu.Lock()
	targets := make([]*browser, 0, len(h.browsers))
	for br := range h.browsers {
		if !transcriptOnly || br.transcript {
			targets = append(targets, br)
		}
	}
	h.mu.Unlock()
	for _, br := range targets {
		h.queueBrowser(br, msg)
	}
}

func (h *Hub) dropBrowser(br *browser) {
	h.mu.Lock()
	_, present := h.browsers[br]
	delete(h.browsers, br)
	n := len(h.browsers)
	h.mu.Unlock()
	br.cancel()
	if present {
		br.conn.Close(websocket.StatusNormalClosure, "")
		h.log.Info("Browser disconnected", "browsers", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// API proxy, dashboard, status
// ─────────────────────────────────────────────────────────────────────────────

func newProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		// Stream large transfers (OTA, files, recordings) as they come.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, `{"status":"error","message":"board unreachable"}`, http.StatusBadGateway)
		},
	}
}

func (h *Hub) serveAPI(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	b := h.board
	h.mu.Unlock()
	if b == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"error","message":"board offline"}`))
		return
	}
	b.proxy.ServeHTTP(w, r)
}

func (h *Hub) serveWeb(w http.ResponseWriter, r *http.Request) {
	if h.cfg.WebDir == "" {
		http.NotFound(w, r)
		return
	}
	// A path that isn't a file gets index.html (the dashboard routes itself).
	p := filepath.Join(h.cfg.WebDir, filepath.FromSlash(filepath.Clean("/"+r.URL.Path)))
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		p = filepath.Join(h.cfg.WebDir, "index.html")
	}
	http.ServeFile(w, r, p)
}

func (h *Hub) serveStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	status := map[string]any{"version": h.cfg.Version, "browsers": len(h.browsers), "voice": h.voiceEnabled(),
		"music": h.music != nil}
	if b := h.board; b != nil {
		status["board"] = map[string]any{
			"id": b.id, "name": b.name, "fw": b.fw, "api": b.apiBase.String(),
			"connected_s": int(time.Since(b.since).Seconds()),
		}
	}
	var edges []map[string]any
	for _, e := range h.edges {
		edges = append(edges, map[string]any{"id": e.id, "name": e.name, "kind": e.kind, "fw": e.fw,
			"roles": e.roles.String(), "connected_s": int(time.Since(e.since).Seconds())})
	}
	status["edges"] = edges
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// ─────────────────────────────────────────────────────────────────────────────

// pinger drops a connection that stops answering pings.
func (h *Hub) pinger(ctx context.Context, conn *websocket.Conn, drop func(error)) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					drop(fmt.Errorf("no pong: %w", err))
				}
				return
			}
		}
	}
}

// writeNow writes one message outside the writer goroutine (handshake only).
func (h *Hub) writeNow(conn *websocket.Conn, msg any) error {
	data, ok := msg.([]byte)
	if !ok {
		data, _ = json.Marshal(msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return fmt.Sprintf("closed %d %s", ce.Code, ce.Reason)
	}
	return strings.TrimSpace(err.Error())
}

// String lists the roles taken, for logs and the status page.
func (r edgeRoles) String() string {
	var names []string
	if r.Voice != nil {
		names = append(names, "voice")
	}
	if r.Player != nil {
		names = append(names, "player")
	}
	if r.Controller {
		names = append(names, "controller")
	}
	if r.State != nil {
		names = append(names, "state")
	}
	return strings.Join(names, ",")
}
