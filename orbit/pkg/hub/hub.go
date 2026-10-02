// Package hub is the board's hub on the LAN (nexus-hub): the waveshare
// board connects to it, and it serves the dashboard to any number of
// browsers. See docs/plans/hub-relay-design.md in the waveshare repository.
//
// Phase 1: the link and the dashboard.
//   - /hub/board: the board's WebSocket. hello (token) -> welcome; then
//     state pushes (JSON without "type", the same as the board's /api/ws)
//     and transcript messages come up, and browser commands go down.
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
	ProtocolVersion = 1
	BoardPath       = "/hub/board"

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
}

type Hub struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	board     *board
	lastState []byte
	browsers  map[*browser]struct{}

	transcript *Transcript
}

type board struct {
	conn    *websocket.Conn
	send    chan []byte
	cancel  context.CancelFunc
	id      string
	name    string
	fw      string
	apiBase *url.URL
	proxy   *httputil.ReverseProxy
	since   time.Time
}

type browser struct {
	conn       *websocket.Conn
	send       chan []byte
	cancel     context.CancelFunc
	transcript bool // subscribed; guarded by Hub.mu
}

type helloMsg struct {
	Type     string `json:"type"`
	Proto    int    `json:"proto"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	FW       string `json:"fw"`
	HTTPPort int    `json:"http_port"`
	Token    string `json:"token"`
}

func New(cfg Config) *Hub {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Hub{
		cfg:        cfg,
		log:        cfg.Log.With("component", "hub"),
		browsers:   map[*browser]struct{}{},
		transcript: NewTranscript(),
	}
}

// Handler serves the board endpoint, the browser socket, the API proxy and
// the dashboard.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(BoardPath, h.serveBoard)
	mux.HandleFunc("/hub/status", h.serveStatus)
	mux.HandleFunc("/api/ws", h.serveBrowser)
	mux.HandleFunc("/api/", h.serveAPI)
	mux.HandleFunc("/", h.serveWeb)
	return mux
}

// ─────────────────────────────────────────────────────────────────────────────
// Board
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

func (h *Hub) serveBoard(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxBoardMessage)

	helloCtx, cancelHello := context.WithTimeout(r.Context(), helloTimeout)
	typ, data, err := conn.Read(helloCtx)
	cancelHello()
	var hello helloMsg
	if err != nil || typ != websocket.MessageText || json.Unmarshal(data, &hello) != nil || hello.Type != "hello" {
		conn.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}
	if !h.tokenOK(hello.Token) {
		h.log.Warn("Board refused: bad token", "id", hello.ID, "addr", r.RemoteAddr)
		h.writeNow(conn, map[string]any{"type": "rejected", "reason": "bad token"})
		conn.Close(closeRejected, "bad token")
		return
	}

	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	port := hello.HTTPPort
	if port == 0 {
		port = 80
	}
	apiBase := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, fmt.Sprint(port))}

	ctx, cancel := context.WithCancel(context.Background())
	b := &board{
		conn: conn, send: make(chan []byte, boardQueueLen), cancel: cancel,
		id: hello.ID, name: hello.Name, fw: hello.FW, apiBase: apiBase,
		proxy: newProxy(apiBase), since: time.Now(),
	}

	h.mu.Lock()
	old := h.board
	h.board = b
	h.lastState = nil
	h.mu.Unlock()
	h.transcript.Reset()
	if old != nil {
		old.conn.Close(closeReplaced, "another board connection")
		old.cancel()
	}

	welcome, _ := json.Marshal(map[string]any{"type": "welcome", "hub": "nexus-hub " + h.cfg.Version, "proto": ProtocolVersion})
	if err := h.writeNow(conn, welcome); err != nil {
		h.dropBoard(b, err)
		return
	}
	h.log.Info("Board connected", "id", b.id, "name", b.name, "fw", b.fw, "api", apiBase.String())

	go h.boardWriter(ctx, b)
	go h.pinger(ctx, conn, func(err error) { h.dropBoard(b, err) })
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			h.dropBoard(b, err)
			return
		}
		if typ == websocket.MessageText {
			h.fromBoard(data)
		}
	}
}

func (h *Hub) fromBoard(data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		h.log.Warn("Unreadable board message", "bytes", len(data))
		return
	}
	switch head.Type {
	case "":
		// A state push: the latest goes to every browser, and to new ones.
		h.mu.Lock()
		h.lastState = data
		h.mu.Unlock()
		h.broadcast(data, false)
	case "transcript":
		if err := h.transcript.Merge(data); err != nil {
			h.log.Warn("Bad transcript message", "err", err)
			return
		}
		h.broadcast(data, true)
	default:
		h.log.Debug("Ignoring board message", "type", head.Type)
	}
}

func (h *Hub) boardWriter(ctx context.Context, b *board) {
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

func (h *Hub) dropBoard(b *board, err error) {
	h.mu.Lock()
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
	if !current {
		return
	}
	h.log.Info("Board disconnected", "id", b.id, "reason", errText(err))
	// The browsers reconnect and wait for the board.
	for _, br := range browsers {
		br.conn.Close(closeBoardOffline, "board offline")
		br.cancel()
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
	status := map[string]any{"version": h.cfg.Version, "browsers": len(h.browsers)}
	if b := h.board; b != nil {
		status["board"] = map[string]any{
			"id": b.id, "name": b.name, "fw": b.fw, "api": b.apiBase.String(),
			"connected_s": int(time.Since(b.since).Seconds()),
		}
	}
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
