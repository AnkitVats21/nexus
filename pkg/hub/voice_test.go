package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeGemini is a Gemini Live endpoint: setupComplete after the setup, a
// short spoken reply to the text "hello", a tool call for "tool" (the reply
// after its answer), and every client message recorded.
type fakeGemini struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	conns   int
	setups  []map[string]any
	inputs  []map[string]any
	toolIDs []string
}

const fakeReplyBytes = 24000 * 2 / 5 // 200 ms

func newFakeGemini(t *testing.T) *fakeGemini {
	g := &fakeGemini{t: t}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGemini) url() string { return "ws" + strings.TrimPrefix(g.srv.URL, "http") }

func (g *fakeGemini) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-goog-api-key") != "gemini-key" || r.URL.RawQuery != "" {
		http.Error(w, "no key", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	g.mu.Lock()
	g.conns++
	g.mu.Unlock()
	ctx := r.Context()
	send := func(v any) { conn.Write(ctx, websocket.MessageBinary, mustJSON(v)) }
	reply := func() {
		audio := base64.StdEncoding.EncodeToString(make([]byte, fakeReplyBytes))
		send(map[string]any{"serverContent": map[string]any{"modelTurn": map[string]any{"parts": []any{
			map[string]any{"inlineData": map[string]any{"mimeType": "audio/pcm;rate=24000", "data": audio}}}}}})
		send(map[string]any{"serverContent": map[string]any{"outputTranscription": map[string]any{"text": "hi there"}}})
		send(map[string]any{"serverContent": map[string]any{"turnComplete": true}})
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		json.Unmarshal(data, &m)
		g.mu.Lock()
		if s, ok := m["setup"].(map[string]any); ok {
			g.setups = append(g.setups, s)
		} else {
			g.inputs = append(g.inputs, m)
		}
		g.mu.Unlock()
		switch {
		case m["setup"] != nil:
			send(map[string]any{"setupComplete": map[string]any{}})
			send(map[string]any{"sessionResumptionUpdate": map[string]any{"newHandle": "h1", "resumable": true}})
		case m["toolResponse"] != nil:
			reply()
		default:
			ri, _ := m["realtimeInput"].(map[string]any)
			switch ri["text"] {
			case "hello":
				send(map[string]any{"serverContent": map[string]any{"inputTranscription": map[string]any{"text": "hello"}}})
				reply()
			case "tool":
				send(map[string]any{"toolCall": map[string]any{"functionCalls": []any{
					map[string]any{"id": "t1", "name": "set_volume", "args": map[string]any{"level": 3}}}}})
			case "music":
				send(map[string]any{"toolCall": map[string]any{"functionCalls": []any{
					map[string]any{"id": "t3", "name": "play", "args": map[string]any{"query": "alpha"}}}}})
			case "remote":
				send(map[string]any{"toolCall": map[string]any{"functionCalls": []any{
					map[string]any{"id": "t2", "name": "get_news", "args": map[string]any{}}}}})
			}
		}
	}
}

func (g *fakeGemini) snapshot() (int, []map[string]any, []map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.conns, append([]map[string]any(nil), g.setups...), append([]map[string]any(nil), g.inputs...)
}

// boardReader reads the board's side of the link: text messages by type,
// voice audio counted.
type boardReader struct {
	t     *testing.T
	conn  *websocket.Conn
	audio int
}

// next returns the next text message whose type is in want; audio frames
// and other messages before it are counted or skipped.
func (br *boardReader) next(ctx context.Context, want ...string) map[string]any {
	br.t.Helper()
	for {
		typ, data, err := br.conn.Read(ctx)
		if err != nil {
			br.t.Fatalf("board read (waiting for %v): %v", want, err)
		}
		if typ == websocket.MessageBinary {
			if data[0] != chanVoice {
				br.t.Fatalf("binary frame on channel %d", data[0])
			}
			br.audio += len(data) - 2
			continue
		}
		var m map[string]any
		json.Unmarshal(data, &m)
		for _, w := range want {
			if m["type"] == w {
				return m
			}
		}
	}
}

func boardSetup() map[string]any {
	return map[string]any{
		"model":                    "models/gemini-test-live",
		"tools":                    []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "set_volume"}}}},
		"contextWindowCompression": map[string]any{"slidingWindow": map[string]any{}},
	}
}

func newVoiceHub(t *testing.T, g *fakeGemini, mcp *MCPClient) *httptest.Server {
	h, err := New(Config{Version: "test", Tokens: []string{testToken}, GeminiKey: "gemini-key",
		GeminiURL: g.url(), MCP: mcp, VoiceLead: 100 * time.Millisecond, NoAuth: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestVoiceSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newVoiceHub(t, g, nil)

	conn, welcome := connectBoard(t, ctx, srv.URL, nil, testToken)
	if v, _ := welcome["voice"].(map[string]any); v["down"] != "pcm" {
		t.Fatalf("welcome voice: %v", welcome["voice"])
	}
	br := &boardReader{t: t, conn: conn}

	// A session with a first text turn: ready, then the reply's audio, then
	// turn_complete after all of it.
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "keepalive_s": 60,
		"resume_min": 60, "text": "hello"})
	br.next(ctx, "session_ready")
	if m := br.next(ctx, "transcription"); m["role"] != "user" {
		t.Fatalf("first transcription: %v", m)
	}
	br.next(ctx, "turn_complete")
	if br.audio != fakeReplyBytes {
		t.Fatalf("reply audio %d bytes before turn_complete, want %d", br.audio, fakeReplyBytes)
	}

	// Mic audio goes up as realtimeInput.
	conn.Write(ctx, websocket.MessageBinary, append([]byte{chanMic, 0}, make([]byte, 640)...))

	// A board skill: tool_call down, tool_response up, then the reply.
	write(t, ctx, conn, map[string]any{"type": "text_turn", "text": "tool"})
	call := br.next(ctx, "tool_call")
	if call["id"] != "t1" || call["name"] != "set_volume" {
		t.Fatalf("tool_call: %v", call)
	}
	write(t, ctx, conn, map[string]any{"type": "tool_response", "id": "t1", "result": map[string]any{"status": "ok"}})
	br.next(ctx, "turn_complete")

	_, setups, inputs := g.snapshot()
	if len(setups) != 1 || setups[0]["model"] != "models/gemini-test-live" {
		t.Fatalf("setups: %v", setups)
	}
	var mic, toolResp bool
	for _, in := range inputs {
		if ri, _ := in["realtimeInput"].(map[string]any); ri["audio"] != nil {
			mic = true
		}
		if tr, _ := in["toolResponse"].(map[string]any); tr != nil {
			fr := tr["functionResponses"].([]any)[0].(map[string]any)
			out := fr["response"].(map[string]any)["output"].(map[string]any)
			toolResp = fr["id"] == "t1" && out["status"] == "ok"
		}
	}
	if !mic || !toolResp {
		t.Fatalf("gemini inputs (mic %v, tool response %v): %v", mic, toolResp, inputs)
	}

	// End and start again within keepalive_s: the same Gemini connection.
	write(t, ctx, conn, map[string]any{"type": "session_end"})
	time.Sleep(50 * time.Millisecond)
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "keepalive_s": 60,
		"resume_min": 60, "text": "hello"})
	br.next(ctx, "session_ready")
	br.next(ctx, "turn_complete")
	if conns, _, _ := g.snapshot(); conns != 1 {
		t.Fatalf("gemini connections %d, want 1 (kept)", conns)
	}

	// This session still takes the kept connection, and with keepalive 0 its
	// end closes it; the next session resumes the conversation with the handle.
	write(t, ctx, conn, map[string]any{"type": "session_end"})
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "keepalive_s": 0,
		"resume_min": 60})
	br.next(ctx, "session_ready")
	write(t, ctx, conn, map[string]any{"type": "session_end"})
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "keepalive_s": 0,
		"resume_min": 60})
	if m := br.next(ctx, "session_ready"); m["resumed"] != true {
		t.Fatalf("second connection not resumed: %v", m)
	}
	conns, setups, _ := g.snapshot()
	if conns != 2 {
		t.Fatalf("gemini connections %d, want 2", conns)
	}
	if r, _ := setups[1]["sessionResumption"].(map[string]any); r["handle"] != "h1" {
		t.Fatalf("resumption: %v", setups[1]["sessionResumption"])
	}
}

func TestVoiceQueueFlush(t *testing.T) {
	q := &voiceQueue{lead: 0}
	q.cond = sync.NewCond(&q.mu)
	q.pushPCM(make([]byte, voiceChunkBytes*3+10))
	if len(q.items) != 4 {
		t.Fatalf("chunks %d, want 4", len(q.items))
	}
	q.flush()
	q.pushText([]byte(`{"type":"interrupted"}`))
	if len(q.items) != 1 || q.items[0].text == nil {
		t.Fatalf("after flush: %d items", len(q.items))
	}
}

func TestVoicePacing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newVoiceHub(t, g, nil)
	conn, _ := connectBoard(t, ctx, srv.URL, nil, testToken)
	br := &boardReader{t: t, conn: conn}
	t0 := time.Now()
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "text": "hello"})
	br.next(ctx, "turn_complete")
	// 200 ms of audio with a 100 ms lead: at least ~100 ms to send.
	if el := time.Since(t0); el < 80*time.Millisecond {
		t.Fatalf("200 ms of audio sent in %v: not paced", el)
	}
}

func TestVoiceMCPTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var calls []string
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mcp-token" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &req)
		mu.Lock()
		calls = append(calls, req.Method)
		mu.Unlock()
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			result = map[string]any{"protocolVersion": mcpProtocolVersion}
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "get_news", "description": "News", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "music_track", "_meta": map[string]any{"nexus/device_only": true}},
				map[string]any{"name": "set_volume", "description": "collides with a board skill"},
			}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "headline"}}}
		}
		// Answer as server-sent events, as streamable HTTP servers may.
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message\ndata: "+string(mustJSON(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result}))+"\n\n")
	}))
	defer mcpSrv.Close()

	g := newFakeGemini(t)
	mcp := NewMCPClient(mcpSrv.URL, "mcp-token", 0, nil)
	if err := mcp.Refresh(ctx); err != nil {
		t.Fatalf("mcp refresh: %v", err)
	}
	srv := newVoiceHub(t, g, mcp)
	conn, _ := connectBoard(t, ctx, srv.URL, nil, testToken)
	br := &boardReader{t: t, conn: conn}
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "text": "remote"})
	br.next(ctx, "session_ready")
	br.next(ctx, "reply_owed")
	br.next(ctx, "turn_complete") // after the hub answered get_news itself

	_, setups, inputs := g.snapshot()
	decls := setups[0]["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	var names []string
	for _, d := range decls {
		names = append(names, d.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "set_volume,get_news" {
		t.Fatalf("declarations: %v", names)
	}
	var out map[string]any
	for _, in := range inputs {
		if tr, _ := in["toolResponse"].(map[string]any); tr != nil {
			fr := tr["functionResponses"].([]any)[0].(map[string]any)
			out = fr["response"].(map[string]any)["output"].(map[string]any)
		}
	}
	if out["result"] != "headline" {
		t.Fatalf("tool response: %v", out)
	}
}

func TestVoiceSearchSetup(t *testing.T) {
	setup := boardSetup()
	setup["model"] = "models/gemini-2.5-flash-live"
	setup["tools"] = []any{map[string]any{"functionDeclarations": []any{
		map[string]any{"name": "set_volume"}, map[string]any{"name": "get_weather"}}}}
	setup["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": "Be brief."}}}
	h, err := New(Config{GeminiKey: "k", NoAuth: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v := h.voiceFor("x")
	v.start = sessionStartMsg{Setup: setup, WebSearch: true}
	data, gc := v.buildSetupLocked()
	if !gc.search {
		t.Fatal("search off")
	}
	s := string(data)
	if strings.Contains(s, "get_weather") || !strings.Contains(s, "googleSearch") || !strings.Contains(s, "Be brief.\\n\\nYou have Google Search") {
		t.Fatalf("setup: %s", s)
	}
	v.searchRefused[gc.model] = true
	if _, gc2 := v.buildSetupLocked(); gc2.search {
		t.Fatal("search after the model refused it")
	}
}

// end_after_reply follows hub music tools only: a board tool does not get it.
func TestNoEndAfterReplyForBoardTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	srv := newVoiceHub(t, g, nil)
	conn, _ := connectBoard(t, ctx, srv.URL, nil, testToken)
	write(t, ctx, conn, map[string]any{"type": "session_start", "setup": boardSetup(), "keepalive_s": 60, "resume_min": 60})
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "session_ready") {
			break
		}
	}
	write(t, ctx, conn, map[string]any{"type": "text_turn", "text": "tool"})
	write(t, ctx, conn, map[string]any{"type": "tool_response", "id": "t1", "result": map[string]any{"status": "ok"}})
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageBinary {
			continue
		}
		if strings.Contains(string(data), "end_after_reply") {
			t.Fatalf("end_after_reply after a board tool: %s", data)
		}
		if strings.Contains(string(data), "turn_complete") {
			return
		}
	}
}
