package bluetooth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// API serves the dashboard's Bluetooth card (through the hub's
// /hub/bluetooth proxy; Orbit listens on localhost only):
//
//	GET  /bluetooth             adapter, devices, the action running, its last error
//	POST /bluetooth/scan        {"seconds": 20} discovery (5-60 s)
//	POST /bluetooth/pair        {"mac"} pair, trust, connect
//	POST /bluetooth/connect     {"mac"}
//	POST /bluetooth/disconnect  {"mac"}
//	POST /bluetooth/remove      {"mac"} forget it
//	POST /bluetooth/power       {"on": true}
//
// Scan, pair and connect take seconds: they run in the background, one at a
// time (409 while one runs), and GET shows "busy" until they finish.
type API struct {
	Ctl *Ctl
	// Active reports the speaker Orbit is playing on, for the card.
	Active func() (Device, bool)

	mu      sync.Mutex
	busy    string // "scan", "pair <mac>", ...
	lastErr string
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bluetooth", a.status)
	for _, action := range []string{"scan", "pair", "connect", "disconnect", "remove", "power"} {
		mux.HandleFunc("POST /bluetooth/"+action, a.action(action))
	}
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"status": "error", "message": msg})
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ad, err := a.Ctl.Adapter(ctx)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "no Bluetooth adapter: "+err.Error())
		return
	}
	devs, err := a.Ctl.Devices(ctx, "")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if devs == nil {
		devs = []Device{}
	}
	a.mu.Lock()
	resp := map[string]any{"status": "ok", "adapter": ad, "devices": devs, "busy": a.busy, "last_error": a.lastErr}
	a.mu.Unlock()
	if a.Active != nil {
		if d, ok := a.Active(); ok {
			resp["active"] = d
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) action(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MAC     string `json:"mac"`
			Seconds int    `json:"seconds"`
			On      *bool  `json:"on"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && name != "scan" {
			fail(w, http.StatusBadRequest, "bad JSON")
			return
		}
		body.MAC = strings.ToUpper(strings.TrimSpace(body.MAC))

		var job func(ctx context.Context) error
		label := name
		switch name {
		case "scan":
			secs := body.Seconds
			if secs == 0 {
				secs = 20
			}
			secs = max(5, min(60, secs))
			job = func(ctx context.Context) error { return a.Ctl.Scan(ctx, time.Duration(secs)*time.Second) }
		case "power":
			if body.On == nil {
				fail(w, http.StatusBadRequest, `need "on"`)
				return
			}
			on := *body.On
			job = func(ctx context.Context) error { return a.Ctl.Power(ctx, on) }
		default:
			if !ValidMAC(body.MAC) {
				fail(w, http.StatusBadRequest, `need "mac" (AA:BB:CC:DD:EE:FF)`)
				return
			}
			mac := body.MAC
			label = name + " " + mac
			job = map[string]func(context.Context) error{
				// The speaker chosen here takes over from the one in use
				// (Ctl.OneSpeaker).
				"pair":       func(ctx context.Context) error { a.Ctl.Prefer(mac); return a.Ctl.Pair(ctx, mac) },
				"connect":    func(ctx context.Context) error { a.Ctl.Prefer(mac); return a.Ctl.Connect(ctx, mac) },
				"disconnect": func(ctx context.Context) error { return a.Ctl.Disconnect(ctx, mac) },
				"remove":     func(ctx context.Context) error { return a.Ctl.Remove(ctx, mac) },
			}[name]
		}

		a.mu.Lock()
		if a.busy != "" {
			busy := a.busy
			a.mu.Unlock()
			fail(w, http.StatusConflict, "busy: "+busy)
			return
		}
		a.busy, a.lastErr = label, ""
		a.mu.Unlock()
		go func() {
			err := job(context.Background())
			a.mu.Lock()
			a.busy = ""
			if err != nil {
				a.lastErr = label + ": " + err.Error()
			}
			a.mu.Unlock()
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "started", "busy": label})
	}
}
