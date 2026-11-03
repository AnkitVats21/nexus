package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOrbitBT is Orbit's Bluetooth API (pkg/bluetooth API) over a device
// list: actions run in the background, briefly busy, as on a real adapter.
type fakeOrbitBT struct {
	mu       sync.Mutex
	devices  []btDevice
	nearby   []btDevice // what a scan finds
	busy     string
	lastErr  string
	failPair map[string]bool
	calls    []string // "pair AA:..."
}

func (f *fakeOrbitBT) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "devices": f.devices, "busy": f.busy, "last_error": f.lastErr})
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/bluetooth/")
	var body struct {
		MAC string `json:"mac"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if f.busy != "" {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "busy: " + f.busy})
		return
	}
	f.calls = append(f.calls, strings.TrimSpace(action+" "+body.MAC))
	f.busy, f.lastErr = action, ""
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.busy = ""
		if action == "scan" {
			f.devices = append(f.devices, f.nearby...)
			return
		}
		for i := range f.devices {
			d := &f.devices[i]
			if d.MAC != body.MAC {
				continue
			}
			switch action {
			case "pair":
				if f.failPair[d.MAC] {
					f.lastErr = "pair " + d.MAC + ": pairing failed: Failed to pair: org.bluez.Error.AuthenticationTimeout"
					return
				}
				d.Paired, d.Connected = true, true
			case "connect":
				d.Connected = true
			case "disconnect":
				d.Connected = false
			}
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"status": "started"})
}

func (f *fakeOrbitBT) callLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ", ")
}

// The devices as the HP knows them: a soundbar, a speaker, a mouse.
func homeDevices() []btDevice {
	return []btDevice{
		{MAC: "F4:4E:FD:4D:71:6C", Name: "Aavante Bar 1550", Paired: true, Audio: true},
		{MAC: "CB:4E:FC:8F:A9:0C", Name: "Tribit StormBox 2", Paired: true, Audio: true},
		{MAC: "D1:02:77:B2:9B:55", Name: "Logi M196", Paired: true, Connected: true},
	}
}

func newTestBluetooth(t *testing.T, f *fakeOrbitBT) *Bluetooth {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	api, _ := url.Parse(srv.URL)
	b := newBluetooth(api)
	b.poll = 5 * time.Millisecond
	return b
}

func btCall(t *testing.T, b *Bluetooth, args string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res map[string]any
	if err := json.Unmarshal(b.toolCall(ctx, json.RawMessage(args)), &res); err != nil {
		t.Fatalf("result: %v", err)
	}
	return res
}

func list(v any) string {
	var s []string
	for _, x := range v.([]any) {
		s = append(s, x.(string))
	}
	return strings.Join(s, ",")
}

func TestBluetoothStatus(t *testing.T) {
	f := &fakeOrbitBT{devices: homeDevices()}
	b := newTestBluetooth(t, f)

	// Nothing audio is connected (the mouse is, and is not a speaker).
	res := btCall(t, b, `{"action":"status"}`)
	if res["status"] != "success" || list(res["connected"]) != "" || res["note"] == nil ||
		list(res["known"]) != "Aavante Bar 1550,Tribit StormBox 2" {
		t.Fatalf("status: %v", res)
	}

	f.devices[0].Connected = true
	res = btCall(t, b, `{"action":"status"}`)
	if list(res["connected"]) != "Aavante Bar 1550" || list(res["known"]) != "Tribit StormBox 2" || res["note"] != nil {
		t.Fatalf("status connected: %v", res)
	}
}

func TestBluetoothScan(t *testing.T) {
	f := &fakeOrbitBT{devices: homeDevices(), nearby: []btDevice{
		{MAC: "11:11:11:11:11:01", Name: "JBL Go 3", Audio: true, RSSI: -80},
		{MAC: "11:11:11:11:11:02", Name: "JBL Flip 5", Audio: true, RSSI: -50},
		{MAC: "11:11:11:11:11:03", Name: "Galaxy S21", RSSI: -40},                     // a phone
		{MAC: "11:11:11:11:11:04", Name: "11-11-11-11-11-04", Audio: true, RSSI: -45}, // unnamed
	}}
	b := newTestBluetooth(t, f)
	res := btCall(t, b, `{"action":"scan"}`)
	if res["status"] != "success" || list(res["found"]) != "JBL Flip 5,JBL Go 3" || res["note"] != nil {
		t.Fatalf("scan: %v", res)
	}
	if f.callLog() != "scan" {
		t.Fatalf("calls: %s", f.callLog())
	}

	f.nearby = nil
	f.devices = homeDevices()
	if res := btCall(t, b, `{"action":"scan"}`); list(res["found"]) != "" || res["note"] == nil {
		t.Fatalf("empty scan: %v", res)
	}
}

func TestBluetoothConnect(t *testing.T) {
	f := &fakeOrbitBT{devices: homeDevices()}
	f.devices[0].Connected = true
	b := newTestBluetooth(t, f)

	// A partial name: connect the Tribit, then let go of the soundbar.
	res := btCall(t, b, `{"action":"connect","device":"tribit"}`)
	if res["status"] != "success" || res["connected"] != "Tribit StormBox 2" || res["paired"] != nil {
		t.Fatalf("connect: %v", res)
	}
	if got := f.callLog(); got != "connect CB:4E:FC:8F:A9:0C, disconnect F4:4E:FD:4D:71:6C" {
		t.Fatalf("calls: %s", got)
	}
	if res := btCall(t, b, `{"action":"connect","device":"Tribit StormBox 2"}`); res["note"] == nil {
		t.Fatalf("already connected: %v", res)
	}

	// Words abbreviated ("aav bar"), and the mouse is never a match.
	f.calls = nil
	if res := btCall(t, b, `{"action":"connect","device":"aav bar"}`); res["connected"] != "Aavante Bar 1550" {
		t.Fatalf("prefix words: %v", res)
	}
	if res := btCall(t, b, `{"action":"connect","device":"logi"}`); res["status"] != "not_found" ||
		!strings.Contains(list(res["known"]), "Tribit") {
		t.Fatalf("mouse: %v", res)
	}
}

func TestBluetoothConnectNew(t *testing.T) {
	f := &fakeOrbitBT{devices: append(homeDevices(),
		btDevice{MAC: "11:11:11:11:11:01", Name: "JBL Go 3", Audio: true, RSSI: -80},
		btDevice{MAC: "11:11:11:11:11:02", Name: "JBL Flip 5", Audio: true, RSSI: -50},
		btDevice{MAC: "22:22:22:22:22:01", Name: "Boat Stone", Audio: true, RSSI: -70},
		btDevice{MAC: "22:22:22:22:22:02", Name: "Boat Stone", Audio: true, RSSI: -55},
	), failPair: map[string]bool{"11:11:11:11:11:01": true}}
	b := newTestBluetooth(t, f)

	if res := btCall(t, b, `{"action":"connect","device":"JBL"}`); res["status"] != "ambiguous" ||
		list(res["candidates"]) != "JBL Go 3,JBL Flip 5" {
		t.Fatalf("ambiguous: %v", res)
	}
	// Two of the same name: the closer one is paired.
	res := btCall(t, b, `{"action":"connect","device":"boat stone"}`)
	if res["connected"] != "Boat Stone" || res["paired"] != true || f.callLog() != "pair 22:22:22:22:22:02" {
		t.Fatalf("same name: %v %s", res, f.callLog())
	}
	// A pairing that fails says why.
	res = btCall(t, b, `{"action":"connect","device":"jbl go"}`)
	if res["status"] != "error" || !strings.Contains(res["message"].(string), "AuthenticationTimeout") {
		t.Fatalf("pair failure: %v", res)
	}
}

func TestBluetoothDisconnectAndErrors(t *testing.T) {
	f := &fakeOrbitBT{devices: homeDevices()}
	f.devices[1].Connected = true
	b := newTestBluetooth(t, f)
	if res := btCall(t, b, `{"action":"disconnect"}`); list(res["disconnected"]) != "Tribit StormBox 2" ||
		f.callLog() != "disconnect CB:4E:FC:8F:A9:0C" {
		t.Fatalf("disconnect: %v %s", res, f.callLog())
	}
	if res := btCall(t, b, `{"action":"disconnect"}`); res["note"] == nil {
		t.Fatalf("nothing to disconnect: %v", res)
	}
	if res := btCall(t, b, `{"action":"connect"}`); res["status"] != "error" {
		t.Fatalf("no device: %v", res)
	}
	if res := btCall(t, b, `{"action":"pair"}`); res["status"] != "error" {
		t.Fatalf("bad action: %v", res)
	}

	f.busy = "scan"
	if res := btCall(t, b, `{"action":"scan"}`); !strings.Contains(res["message"].(string), "busy") {
		t.Fatalf("busy: %v", res)
	}

	// Orbit is not running.
	api, _ := url.Parse("http://127.0.0.1:1")
	down := newBluetooth(api)
	if res := btCall(t, down, `{"action":"status"}`); res["status"] != "error" ||
		res["message"] != errNoBluetooth.Error() {
		t.Fatalf("orbit down: %v", res)
	}
}

// The hub offers the tool to Gemini in place of the board's, runs it and
// answers; the session stays open (no end_after_reply).
func TestVoiceBluetoothTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g := newFakeGemini(t)
	f := &fakeOrbitBT{devices: homeDevices()}
	f.devices[0].Connected = true
	orbit := httptest.NewServer(http.HandlerFunc(f.serve))
	defer orbit.Close()
	api, _ := url.Parse(orbit.URL)
	h := New(Config{Version: "test", Tokens: []string{testToken}, GeminiKey: "gemini-key",
		GeminiURL: g.url(), VoiceLead: 100 * time.Millisecond, Tracks: &fakeTracks{}, BluetoothAPI: api})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	board := connectEdge(t, ctx, srv.URL, "board", boardRoles)

	setup := boardSetup()
	decls := setup["tools"].([]any)[0].(map[string]any)
	decls["functionDeclarations"] = append(decls["functionDeclarations"].([]any),
		map[string]any{"name": "bluetooth", "description": "the board's own"})
	write(t, ctx, board.conn, map[string]any{"type": "session_start", "setup": setup, "text": "bluetooth"})
	board.next(ctx, "session_ready")
	if m := board.next(ctx, "reply_owed", "tool_call"); m["type"] != "reply_owed" {
		t.Fatalf("got %v, want reply_owed (the hub runs it)", m)
	}
	if m := board.next(ctx, "turn_complete", "end_after_reply"); m["type"] != "turn_complete" {
		t.Fatalf("got %v: a bluetooth reply keeps the session", m)
	}

	_, setups, inputs := g.snapshot()
	var n int
	for _, d := range setups[0]["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any) {
		if dm := d.(map[string]any); dm["name"] == "bluetooth" {
			n++
			if dm["description"] == "the board's own" {
				t.Fatalf("the board's bluetooth declaration was kept")
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d bluetooth declarations", n)
	}
	var answered bool
	for _, in := range inputs {
		if tr, _ := in["toolResponse"].(map[string]any); tr != nil {
			fr := tr["functionResponses"].([]any)[0].(map[string]any)
			out := fr["response"].(map[string]any)["output"].(map[string]any)
			answered = fr["id"] == "t4" && list(out["connected"]) == "Aavante Bar 1550"
		}
	}
	if !answered {
		t.Fatalf("no bluetooth answer to Gemini: %v", inputs)
	}
}
