package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const testToken = "test-token"

func dial(t *testing.T, ctx context.Context, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return conn
}

func read(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bad JSON %q: %v", data, err)
	}
	return m
}

func write(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// connectBoard connects a fake board whose HTTP server is boardHTTP.
func connectBoard(t *testing.T, ctx context.Context, hubURL string, boardHTTP *httptest.Server, token string) (*websocket.Conn, map[string]any) {
	t.Helper()
	port := 80
	if boardHTTP != nil {
		port, _ = strconv.Atoi(boardHTTP.URL[strings.LastIndex(boardHTTP.URL, ":")+1:])
	}
	conn := dial(t, ctx, "ws"+strings.TrimPrefix(hubURL, "http")+BoardPath)
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 1, "id": "aabbcc", "name": "Nexus", "fw": "test", "http_port": port, "token": token})
	return conn, read(t, ctx, conn)
}

func newTestHub(t *testing.T) (*Hub, *httptest.Server) {
	h := New(Config{Version: "test", Tokens: []string{testToken}})
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return h, srv
}

func TestBadTokenRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, srv := newTestHub(t)
	_, reply := connectBoard(t, ctx, srv.URL, nil, "wrong")
	if reply["type"] != "rejected" {
		t.Fatalf("got %v, want rejected", reply)
	}
}

func TestStateFanOutAndCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, srv := newTestHub(t)
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")

	// No board yet: a browser is turned away.
	early := dial(t, ctx, wsBase+"/api/ws")
	if _, _, err := early.Read(ctx); websocket.CloseStatus(err) != closeBoardOffline {
		t.Fatalf("browser without a board: %v", err)
	}

	boardConn, welcome := connectBoard(t, ctx, srv.URL, nil, testToken)
	if welcome["type"] != "welcome" {
		t.Fatalf("got %v, want welcome", welcome)
	}
	write(t, ctx, boardConn, map[string]any{"connected": true, "up": 1})

	// Both browsers get the state; the second one connects after it was sent.
	b1 := dial(t, ctx, wsBase+"/api/ws")
	if got := read(t, ctx, b1); got["up"] != float64(1) {
		t.Fatalf("browser 1 state: %v", got)
	}
	b2 := dial(t, ctx, wsBase+"/api/ws")
	if got := read(t, ctx, b2); got["up"] != float64(1) {
		t.Fatalf("browser 2 state: %v", got)
	}
	write(t, ctx, boardConn, map[string]any{"connected": true, "up": 2})
	for i, b := range []*websocket.Conn{b1, b2} {
		if got := read(t, ctx, b); got["up"] != float64(2) {
			t.Fatalf("browser %d push: %v", i+1, got)
		}
	}

	// A browser command reaches the board as sent.
	write(t, ctx, b2, map[string]any{"cmd": "volume", "value": 40})
	if got := read(t, ctx, boardConn); got["cmd"] != "volume" || got["value"] != float64(40) {
		t.Fatalf("board got %v", got)
	}

	// Transcript: kept by the hub, sent whole on subscribe, deltas after.
	write(t, ctx, boardConn, map[string]any{"type": "transcript", "seq": 2, "entries": []any{
		map[string]any{"id": 1, "seq": 1, "role": "user", "text": "hi"},
		map[string]any{"id": 2, "seq": 2, "role": "model", "text": "hel"},
	}})
	time.Sleep(50 * time.Millisecond) // let the hub merge it
	write(t, ctx, b1, map[string]any{"cmd": "subscribe", "transcript": true})
	full := read(t, ctx, b1)
	if full["type"] != "transcript" || len(full["entries"].([]any)) != 2 {
		t.Fatalf("full transcript: %v", full)
	}
	write(t, ctx, boardConn, map[string]any{"type": "transcript", "seq": 3, "entries": []any{
		map[string]any{"id": 2, "seq": 3, "role": "model", "text": "hello"},
	}})
	if got := read(t, ctx, b1); got["seq"] != float64(3) {
		t.Fatalf("delta: %v", got)
	}
	// b2 didn't subscribe: its next frame is the next state push.
	write(t, ctx, boardConn, map[string]any{"connected": true, "up": 3})
	for i, b := range []*websocket.Conn{b1, b2} {
		if got := read(t, ctx, b); got["up"] != float64(3) {
			t.Fatalf("browser %d got %v", i+1, got)
		}
	}

	// The board drops: browsers are closed so they reconnect and wait.
	boardConn.Close(websocket.StatusNormalClosure, "")
	if _, _, err := b1.Read(ctx); websocket.CloseStatus(err) != closeBoardOffline {
		t.Fatalf("browser after the board left: %v", err)
	}
}

func TestAPIProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boardHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(body))
	}))
	defer boardHTTP.Close()
	_, srv := newTestHub(t)

	if res, _ := http.Get(srv.URL + "/api/music/status"); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no board: status %d", res.StatusCode)
	}

	// The hub forwards to the board's address as the hub sees it (127.0.0.1 here).
	connectBoard(t, ctx, srv.URL, boardHTTP, testToken)
	res, err := http.Post(srv.URL+"/api/alarms?x=1", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != `POST /api/alarms?x=1 {"a":1}` {
		t.Fatalf("proxied: %q", body)
	}
}

func TestDashboardFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("index"), 0o644)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("js"), 0o644)
	h := New(Config{Version: "test", WebDir: dir})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	for path, want := range map[string]string{"/": "index", "/assets/app.js": "js", "/library": "index", "/../etc/passwd": "index"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		if string(body) != want {
			t.Errorf("%s: got %q, want %q", path, body, want)
		}
	}
}

func TestTranscriptCap(t *testing.T) {
	tr := NewTranscript()
	for i := 1; i <= MaxTranscriptEntries+5; i++ {
		msg, _ := json.Marshal(map[string]any{"seq": i, "entries": []any{map[string]any{"id": i, "text": "x"}}})
		if err := tr.Merge(msg); err != nil {
			t.Fatal(err)
		}
	}
	var full transcriptMsg
	json.Unmarshal(tr.Full(), &full)
	if len(full.Entries) != MaxTranscriptEntries || full.Seq != MaxTranscriptEntries+5 {
		t.Fatalf("entries %d seq %d", len(full.Entries), full.Seq)
	}
	var first entryID
	json.Unmarshal(full.Entries[0], &first)
	if first.ID != 6 {
		t.Fatalf("oldest kept id %d, want 6", first.ID)
	}
}

func TestValidTextDropsCutCharacter(t *testing.T) {
	// "भज" with its last character cut after the first byte, as the
	// board's fixed-size title buffer leaves it.
	msg := []byte("{\"title\":\"भ\xe0\",\"artist\":\"x\"}")
	got := validText(msg)
	if !utf8.Valid(got) || string(got) != "{\"title\":\"भ\",\"artist\":\"x\"}" {
		t.Fatalf("got %q", got)
	}
	ok := []byte(`{"a":"b"}`)
	if g := validText(ok); &g[0] != &ok[0] {
		t.Fatal("copied a valid frame")
	}
}
