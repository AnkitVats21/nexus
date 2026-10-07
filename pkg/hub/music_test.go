package hub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeTracks: songs a..f; each one's recommendations are the ones after it.
// "gone" is not found.
type fakeTracks struct {
	mu      sync.Mutex
	lookups []string
}

var fakeSongs = []Track{
	{ID: "a", Title: "Alpha", Artist: "X", Duration: 200},
	{ID: "b", Title: "Bravo", Artist: "X", Duration: 200},
	{ID: "c", Title: "Charlie", Artist: "Y", Duration: 200},
	{ID: "d", Title: "Delta", Artist: "Y", Duration: 200},
}

func (f *fakeTracks) Name() string { return "fake" }

func (f *fakeTracks) Search(_ context.Context, query string, limit int) ([]Track, error) {
	for _, t := range fakeSongs {
		if strings.Contains(strings.ToLower(t.Title), strings.ToLower(query)) {
			return []Track{t}, nil
		}
	}
	return nil, ErrTrackNotFound
}

func (f *fakeTracks) Lookup(ctx context.Context, id, query string, stream bool, recs int) (TrackInfo, error) {
	f.mu.Lock()
	f.lookups = append(f.lookups, id+"|"+query)
	f.mu.Unlock()
	if id == "" {
		found, err := f.Search(ctx, query, 1)
		if err != nil {
			return TrackInfo{}, err
		}
		id = found[0].ID
	}
	for i, t := range fakeSongs {
		if t.ID == id {
			info := TrackInfo{Track: t}
			if stream {
				info.URL = "https://cdn.test/" + id
			}
			for _, r := range fakeSongs[i+1:] {
				if len(info.Recs) < recs {
					info.Recs = append(info.Recs, r)
				}
			}
			return info, nil
		}
	}
	return TrackInfo{}, ErrTrackNotFound
}

func newMusicHub(t *testing.T, g *fakeGemini) *httptest.Server {
	cfg := Config{Version: "test", Tokens: []string{testToken}, Tracks: &fakeTracks{}, VoiceLead: 100 * time.Millisecond}
	if g != nil {
		cfg.GeminiKey, cfg.GeminiURL = "gemini-key", g.url()
	}
	srv := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(srv.Close)
	return srv
}

var (
	boardRoles = map[string]any{"voice": map[string]any{}, "controller": true,
		"player": map[string]any{"url": true, "queue_copy": true}}
	orbitRoles = map[string]any{"controller": true, "player": map[string]any{"url": true, "resolves": true}}
)

// connectEdge connects a proto 2 edge and returns a reader past the welcome.
func connectEdge(t *testing.T, ctx context.Context, hubURL, id string, roles map[string]any) *boardReader {
	t.Helper()
	conn := dial(t, ctx, "ws"+strings.TrimPrefix(hubURL, "http")+EdgePath)
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 2, "id": id, "name": id, "kind": "test",
		"token": testToken, "roles": roles})
	w := read(t, ctx, conn)
	if w["type"] != "welcome" || w["music"] != true || w["proto"] != float64(2) {
		t.Fatalf("welcome: %v", w)
	}
	return &boardReader{t: t, conn: conn}
}

func TestMusicPlayAndAdvance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := newMusicHub(t, nil)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)

	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	p := board.next(ctx, "music_play")
	if p["track"].(map[string]any)["id"] != "a" || p["url"] != "https://cdn.test/a" || p["seq"] != float64(1) {
		t.Fatalf("music_play: %v", p)
	}
	q := board.next(ctx, "music_queue")
	if tracks := q["tracks"].([]any); len(tracks) != 3 || tracks[0].(map[string]any)["id"] != "b" {
		t.Fatalf("music_queue: %v", q)
	}
	write(t, ctx, board.conn, map[string]any{"type": "music_report", "seq": 1, "id": "a", "state": "playing", "position_ms": 1000})
	if s := board.next(ctx, "music_state"); s["state"] == nil {
		t.Fatalf("music_state: %v", s)
	}
	time.Sleep(50 * time.Millisecond) // the next song's URL is fetched ahead

	// The song ends: the next one, with its URL.
	write(t, ctx, board.conn, map[string]any{"type": "music_event", "seq": 1, "id": "a", "event": "ended"})
	p = board.next(ctx, "music_play")
	if p["track"].(map[string]any)["id"] != "b" || p["url"] != "https://cdn.test/b" || p["seq"] != float64(2) {
		t.Fatalf("second music_play: %v", p)
	}

	// A late "ended" for the song before is ignored; a skip moves on.
	write(t, ctx, board.conn, map[string]any{"type": "music_event", "seq": 1, "id": "a", "event": "ended"})
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "next"})
	if p = board.next(ctx, "music_play"); p["track"].(map[string]any)["id"] != "c" {
		t.Fatalf("after next: %v", p)
	}
	// previous early in a song goes back a song.
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "previous"})
	if p = board.next(ctx, "music_play"); p["track"].(map[string]any)["id"] != "b" {
		t.Fatalf("after previous: %v", p)
	}
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "pause"})
	board.next(ctx, "music_pause")
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "seek", "position_ms": 42000})
	if s := board.next(ctx, "music_seek"); s["position_ms"] != float64(42000) {
		t.Fatalf("music_seek: %v", s)
	}
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "stop"})
	board.next(ctx, "music_stop")
}

func TestMusicOutputSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := newMusicHub(t, nil)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)
	orbit := connectEdge(t, ctx, srv.URL, "workstation", orbitRoles)

	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	board.next(ctx, "music_play")
	write(t, ctx, board.conn, map[string]any{"type": "music_report", "seq": 1, "id": "a", "state": "playing", "position_ms": 30000})
	time.Sleep(50 * time.Millisecond)

	// To the satellite: it resolves itself (no URL), from the same place.
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "output", "target": "work"})
	board.next(ctx, "music_stop")
	p := orbit.next(ctx, "music_play")
	if p["track"].(map[string]any)["id"] != "a" || p["url"] != nil || p["position_ms"] != float64(30000) {
		t.Fatalf("satellite music_play: %v", p)
	}

	// The satellite goes away: its music stops (no queue copy).
	orbit.conn.Close(websocket.StatusNormalClosure, "")
	for {
		s := board.next(ctx, "music_state")
		if s["output"] == "" && s["state"] == "idle" {
			break
		}
	}
}

func TestMusicSyncAfterAway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := newMusicHub(t, nil)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	board.next(ctx, "music_play")
	board.conn.Close(websocket.StatusNormalClosure, "")

	// Back, having played on alone: its song and queue become the hub's.
	board = connectEdge(t, ctx, srv.URL, "board", boardRoles)
	write(t, ctx, board.conn, map[string]any{"type": "music_sync", "state": "playing", "position_ms": 5000,
		"track": fakeSongs[2], "queue": []Track{fakeSongs[3]}})
	for {
		s := board.next(ctx, "music_state")
		if tr, _ := s["track"].(map[string]any); tr != nil && tr["id"] == "c" && s["output"] == "board" {
			break
		}
	}
	// Its own song reports seq 0; its end plays the next from the queue.
	write(t, ctx, board.conn, map[string]any{"type": "music_event", "seq": 0, "id": "c", "event": "ended"})
	if p := board.next(ctx, "music_play"); p["track"].(map[string]any)["id"] != "d" {
		t.Fatalf("after sync: %v", p)
	}
}

func TestMusicPausedForSessionElsewhere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := newMusicHub(t, nil)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)
	orbit := connectEdge(t, ctx, srv.URL, "workstation", orbitRoles)
	write(t, ctx, orbit.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	orbit.next(ctx, "music_play")
	write(t, ctx, orbit.conn, map[string]any{"type": "music_report", "seq": 1, "id": "a", "state": "playing"})
	time.Sleep(50 * time.Millisecond)

	write(t, ctx, board.conn, map[string]any{"type": "voice_activity", "active": true})
	orbit.next(ctx, "music_pause")
	write(t, ctx, board.conn, map[string]any{"type": "voice_activity", "active": false})
	orbit.next(ctx, "music_resume")
}

func TestMusicVoiceTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newMusicHub(t, g)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)

	setup := boardSetup()
	decls := setup["tools"].([]any)[0].(map[string]any)
	decls["functionDeclarations"] = append(decls["functionDeclarations"].([]any),
		map[string]any{"name": "play", "description": "the board's own"})
	write(t, ctx, board.conn, map[string]any{"type": "session_start", "setup": setup, "text": "music"})
	board.next(ctx, "session_ready")

	// The hub runs it: music_play to the board, not a tool_call.
	if m := board.next(ctx, "music_play", "tool_call"); m["type"] != "music_play" || m["track"].(map[string]any)["id"] != "a" {
		t.Fatalf("got %v, want music_play", m)
	}
	board.next(ctx, "end_after_reply")
	board.next(ctx, "turn_complete")

	_, setups, inputs := g.snapshot()
	var names []string
	for _, d := range setups[0]["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any) {
		dm := d.(map[string]any)
		names = append(names, dm["name"].(string))
		if dm["name"] == "play" && dm["description"] == "the board's own" {
			t.Fatalf("the board's play declaration was kept")
		}
	}
	if strings.Join(names, ",") != "set_volume,play,playback,music_output" {
		t.Fatalf("declarations: %v", names)
	}
	var answered bool
	for _, in := range inputs {
		if tr, _ := in["toolResponse"].(map[string]any); tr != nil {
			fr := tr["functionResponses"].([]any)[0].(map[string]any)
			out := fr["response"].(map[string]any)["output"].(map[string]any)
			answered = fr["id"] == "t3" && out["playing"] == "Alpha by X"
		}
	}
	if !answered {
		t.Fatalf("no play answer to Gemini: %v", inputs)
	}
}

// The board pauses its own music for its own session; moved to the
// satellite during the session, the music starts there held, and plays
// when the session ends.
func TestMusicMovedDuringSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := newMusicHub(t, nil)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)
	orbit := connectEdge(t, ctx, srv.URL, "workstation", orbitRoles)
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	board.next(ctx, "music_play")
	write(t, ctx, board.conn, map[string]any{"type": "music_report", "seq": 1, "id": "a", "state": "playing", "position_ms": 9000})
	time.Sleep(30 * time.Millisecond)

	write(t, ctx, board.conn, map[string]any{"type": "voice_activity", "active": true})
	write(t, ctx, board.conn, map[string]any{"type": "music_report", "seq": 1, "id": "a", "state": "paused", "position_ms": 9500})
	time.Sleep(30 * time.Millisecond)
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "output", "target": "workstation"})
	if p := orbit.next(ctx, "music_play"); p["paused"] != true {
		t.Fatalf("satellite play during the session: %v", p)
	}
	write(t, ctx, board.conn, map[string]any{"type": "voice_activity", "active": false})
	orbit.next(ctx, "music_resume")
}

// cdnTracks: fakeTracks whose stream URLs are a real (test) server.
type cdnTracks struct {
	fakeTracks
	cdn string
}

func (c *cdnTracks) Lookup(ctx context.Context, id, query string, stream bool, recs int) (TrackInfo, error) {
	info, err := c.fakeTracks.Lookup(ctx, id, query, stream, recs)
	if err == nil && info.URL != "" {
		info.URL = c.cdn + "/" + info.ID
	}
	return info, err
}

func TestMusicCache(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	song := make([]byte, 100<<10)
	for i := range song {
		song[i] = byte(i)
	}
	var sawRange atomic.Bool
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange.Store(r.Header.Get("Range") == "bytes=0-")
		w.Write(song)
	}))
	t.Cleanup(cdn.Close)
	cache, err := NewMusicCache(t.TempDir(), 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Version: "test", Tokens: []string{testToken}, Tracks: &cdnTracks{cdn: cdn.URL}, Cache: cache})
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)

	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	if p := board.next(ctx, "music_play"); p["url"] != cdn.URL+"/a" {
		t.Fatalf("first play: %v", p["url"])
	}
	for !cache.Has("a") {
		if ctx.Err() != nil {
			t.Fatal("a was never cached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawRange.Load() {
		t.Error("the download had no Range header (YouTube throttles without)")
	}
	// Played again: from the hub.
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "id": "a"})
	p := board.next(ctx, "music_play")
	want := srv.URL + CachePath + "a.webm"
	if p["url"] != want {
		t.Fatalf("second play: %v, want %s", p["url"], want)
	}
	req, _ := http.NewRequest("GET", want, nil)
	req.Header.Set("Range", "bytes=1000-1999")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 1000 || body[0] != song[1000] {
		t.Fatalf("range: %d, %d bytes", resp.StatusCode, len(body))
	}
	// Over the size: the least recently used goes.
	small, _ := NewMusicCache(t.TempDir(), 250<<10, nil)
	for _, id := range []string{"x1", "x2", "x3"} {
		small.Fetch(id, cdn.URL+"/"+id)
		for !small.Has(id) {
			if ctx.Err() != nil {
				t.Fatalf("%s was never cached", id)
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		if id == "x2" {
			small.Has("x1") // used again: x2 is now the oldest
		}
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(small.path("x2")); err == nil {
		t.Error("x2 (least recently used) should have been evicted")
	}
	if !small.Has("x1") || !small.Has("x3") {
		t.Error("x1 and x3 should be kept")
	}
}

func TestResolveOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	song := make([]byte, 100<<10)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(song)
	}))
	t.Cleanup(cdn.Close)

	cacheDir := t.TempDir()
	cache, err := NewMusicCache(cacheDir, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}

	h := New(Config{
		Version:     "test",
		Tokens:      []string{testToken},
		Tracks:      &cdnTracks{cdn: cdn.URL},
		Cache:       cache,
		ResolveOnly: true,
	})
	if h.cfg.Cache != nil {
		t.Fatalf("expected h.cfg.Cache to be nil in resolve-only mode, got %v", h.cfg.Cache)
	}

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	// Verify /hub/cache/ is not mounted (returns 404).
	cacheResp, err := http.Get(srv.URL + CachePath + "a.webm")
	if err != nil {
		t.Fatal(err)
	}
	cacheResp.Body.Close()
	if cacheResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected /hub/cache/ to return 404 in resolve-only mode, got %d", cacheResp.StatusCode)
	}

	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)

	// First play: resolves directly to CDN.
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "query": "alpha"})
	p := board.next(ctx, "music_play")
	if p["url"] != cdn.URL+"/a" {
		t.Fatalf("first play url = %v, want %v", p["url"], cdn.URL+"/a")
	}

	// Settle briefly and ensure no cache download occurs.
	time.Sleep(50 * time.Millisecond)
	if cache.Has("a") {
		t.Fatal("song 'a' should not be cached in resolve-only mode")
	}
	entries, _ := os.ReadDir(cacheDir)
	if len(entries) > 0 {
		t.Fatalf("cache directory should remain empty, found %d entries", len(entries))
	}

	// Second play: plays again and still resolves directly to CDN, not from hub cache.
	write(t, ctx, board.conn, map[string]any{"type": "music_action", "action": "play", "id": "a"})
	p2 := board.next(ctx, "music_play")
	if p2["url"] != cdn.URL+"/a" {
		t.Fatalf("second play url = %v, want direct CDN %v", p2["url"], cdn.URL+"/a")
	}
}
