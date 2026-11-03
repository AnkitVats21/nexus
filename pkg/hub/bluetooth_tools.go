package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ─────────────────────────────────────────────────────────────────────────────
// The bluetooth voice tool: which speaker the hub's machine plays on, and
// scan / connect / disconnect by name. It drives Orbit's Bluetooth API on
// this machine (pkg/bluetooth, Config.BluetoothAPI). The model only sees
// speaker names; the hub resolves a name to a device, so it never has to
// read out or repeat a MAC address.

const bluetoothToolName = "bluetooth"

const (
	btScanSeconds = 10
	btMaxFound    = 6
	// Pairing a new speaker runs pair, trust and connect (pkg/bluetooth
	// Pair): up to about 90 s when the speaker is slow to answer.
	btToolTimeout = 100 * time.Second
)

func bluetoothToolDeclaration() map[string]any {
	str := func(desc string, enum ...string) map[string]any {
		p := map[string]any{"type": "STRING", "description": desc}
		if len(enum) > 0 {
			p["enum"] = enum
		}
		return p
	}
	return map[string]any{
		"name": bluetoothToolName,
		"description": "The Bluetooth speakers of the house speaker (headphones count too). " +
			"'status' says which one is connected; when none is, tell the user the known ones and ask which to connect, or offer to look for new ones. " +
			"'scan' looks for new speakers nearby (about 10 seconds): read out the names found and ask which to connect. " +
			"'connect' connects one by the name the user said (part of the name is enough); a new one is paired first. " +
			"When the result lists candidates, ask the user which one they meant. 'disconnect' disconnects the speaker.",
		"parameters": map[string]any{"type": "OBJECT", "required": []string{"action"}, "properties": map[string]any{
			"action": str("status, scan, connect or disconnect.", "status", "scan", "connect", "disconnect"),
			"device": str("The speaker's name as the user said it, for connect (and optionally disconnect).")}},
	}
}

// btDevice is a device as Orbit's API reports it (pkg/bluetooth Device).
type btDevice struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	Paired    bool   `json:"paired"`
	Connected bool   `json:"connected"`
	Audio     bool   `json:"audio"`
	RSSI      int    `json:"rssi"`
}

type btStatus struct {
	Status    string     `json:"status"`
	Message   string     `json:"message"`
	Devices   []btDevice `json:"devices"`
	Busy      string     `json:"busy"`
	LastError string     `json:"last_error"`
}

// Bluetooth runs the bluetooth tool against Orbit's API.
type Bluetooth struct {
	api    *url.URL
	client *http.Client
	poll   time.Duration // between checks while Orbit is busy

	mu sync.Mutex // one voice command at a time
}

func newBluetooth(api *url.URL) *Bluetooth {
	return &Bluetooth{api: api, client: &http.Client{Timeout: 25 * time.Second}, poll: 500 * time.Millisecond}
}

func (b *Bluetooth) toolCall(ctx context.Context, args json.RawMessage) json.RawMessage {
	var a struct {
		Action string `json:"action"`
		Device string `json:"device"`
	}
	json.Unmarshal(args, &a)
	b.mu.Lock()
	defer b.mu.Unlock()
	var res map[string]any
	var err error
	switch a.Action {
	case "status":
		res, err = b.status(ctx)
	case "scan":
		res, err = b.scan(ctx)
	case "connect":
		res, err = b.connect(ctx, a.Device)
	case "disconnect":
		res, err = b.disconnect(ctx, a.Device)
	default:
		return toolError("action must be status, scan, connect or disconnect")
	}
	if err != nil {
		return toolError(err.Error())
	}
	data, _ := json.Marshal(res)
	return data
}

func (b *Bluetooth) status(ctx context.Context) (map[string]any, error) {
	st, err := b.get(ctx)
	if err != nil {
		return nil, err
	}
	sp := speakers(st.Devices)
	res := map[string]any{"status": "success", "connected": speakerNames(sp, connectedOnly), "known": speakerNames(sp, pairedIdle)}
	if len(speakerNames(sp, connectedOnly)) == 0 {
		res["note"] = "No speaker is connected."
	}
	return res, nil
}

func (b *Bluetooth) scan(ctx context.Context) (map[string]any, error) {
	if err := b.run(ctx, "scan", map[string]any{"seconds": btScanSeconds}); err != nil {
		return nil, err
	}
	st, err := b.get(ctx)
	if err != nil {
		return nil, err
	}
	sp := speakers(st.Devices)
	var found []btDevice
	for _, d := range sp {
		if !d.Paired {
			found = append(found, d)
		}
	}
	sortByRSSI(found)
	if len(found) > btMaxFound {
		found = found[:btMaxFound]
	}
	res := map[string]any{"status": "success", "found": speakerNames(found, nil), "known": speakerNames(sp, pairedIdle)}
	if len(found) == 0 {
		res["note"] = "No new speakers found: the speaker must be in pairing mode (often a long press on its Bluetooth button). Then scan again."
	}
	return res, nil
}

func (b *Bluetooth) connect(ctx context.Context, query string) (map[string]any, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("device is required: the speaker's name")
	}
	st, err := b.get(ctx)
	if err != nil {
		return nil, err
	}
	sp := speakers(st.Devices)
	cands := matchSpeakers(query, sp)
	switch {
	case len(cands) == 0:
		return map[string]any{"status": "not_found", "message": "No speaker by that name.",
			"known": speakerNames(sp, pairedIdle), "hint": "Offer a scan for new speakers."}, nil
	case len(cands) > 1 && !sameName(cands):
		return map[string]any{"status": "ambiguous", "candidates": speakerNames(cands, nil),
			"message": "Ask the user which one they meant."}, nil
	}
	d := pick(cands)
	if d.Connected {
		return map[string]any{"status": "success", "connected": d.Name, "note": "It was already connected."}, nil
	}
	action := "connect"
	if !d.Paired {
		action = "pair"
	}
	if err := b.run(ctx, action, map[string]any{"mac": d.MAC}); err != nil {
		return nil, fmt.Errorf("could not connect to %s: %v", d.Name, err)
	}
	// One speaker at a time: the music follows the new one.
	for _, o := range sp {
		if o.Connected && o.MAC != d.MAC {
			b.run(ctx, "disconnect", map[string]any{"mac": o.MAC})
		}
	}
	res := map[string]any{"status": "success", "connected": d.Name}
	if !d.Paired {
		res["paired"] = true
	}
	return res, nil
}

func (b *Bluetooth) disconnect(ctx context.Context, query string) (map[string]any, error) {
	st, err := b.get(ctx)
	if err != nil {
		return nil, err
	}
	var conn []btDevice
	for _, d := range speakers(st.Devices) {
		if d.Connected {
			conn = append(conn, d)
		}
	}
	if strings.TrimSpace(query) != "" {
		conn = matchSpeakers(query, conn)
	}
	if len(conn) == 0 {
		return map[string]any{"status": "success", "note": "No speaker was connected."}, nil
	}
	for _, d := range conn {
		if err := b.run(ctx, "disconnect", map[string]any{"mac": d.MAC}); err != nil {
			return nil, fmt.Errorf("could not disconnect %s: %v", d.Name, err)
		}
	}
	return map[string]any{"status": "success", "disconnected": speakerNames(conn, nil)}, nil
}

// ── Orbit's API ─────────────────────────────────────────────────────────────

var errNoBluetooth = errors.New("Bluetooth is not available right now")

func (b *Bluetooth) get(ctx context.Context) (btStatus, error) {
	var st btStatus
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.api.JoinPath("bluetooth").String(), nil)
	resp, err := b.client.Do(req)
	if err != nil {
		return st, errNoBluetooth
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || st.Status != "ok" {
		if st.Message != "" {
			return st, errors.New(st.Message)
		}
		return st, errNoBluetooth
	}
	return st, nil
}

// run starts an action and waits until Orbit has finished it; the error is
// the action's own (last_error) or why it could not start.
func (b *Bluetooth) run(ctx context.Context, action string, body map[string]any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.api.JoinPath("bluetooth", action).String(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return errNoBluetooth
	}
	var r struct {
		Message string `json:"message"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
	case http.StatusConflict:
		return errors.New("Bluetooth is busy with something else; try again in a moment")
	default:
		if r.Message == "" {
			r.Message = resp.Status
		}
		return errors.New(r.Message)
	}
	for {
		select {
		case <-ctx.Done():
			return errors.New("it took too long")
		case <-time.After(b.poll):
		}
		st, err := b.get(ctx)
		if err != nil {
			return err
		}
		if st.Busy == "" {
			if st.LastError != "" {
				return errors.New(st.LastError)
			}
			return nil
		}
	}
}

// ── Choosing speakers ───────────────────────────────────────────────────────

var unnamedRe = regexp.MustCompile(`^([0-9A-Fa-f]{2}[-:]){5}[0-9A-Fa-f]{2}$`)

// speakers keeps the audio devices with a name (BlueZ names a device it
// has no name for by its address).
func speakers(devs []btDevice) []btDevice {
	var out []btDevice
	for _, d := range devs {
		if d.Audio && strings.TrimSpace(d.Name) != "" && !unnamedRe.MatchString(d.Name) {
			out = append(out, d)
		}
	}
	return out
}

func connectedOnly(d btDevice) bool { return d.Connected }
func pairedIdle(d btDevice) bool    { return d.Paired && !d.Connected }

func speakerNames(devs []btDevice, keep func(btDevice) bool) []string {
	out := []string{}
	for _, d := range devs {
		if keep == nil || keep(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

// sortByRSSI puts the strongest signal first; 0 (not seen in a scan) last.
func sortByRSSI(devs []btDevice) {
	sort.SliceStable(devs, func(i, j int) bool {
		ri, rj := devs[i].RSSI, devs[j].RSSI
		if ri == 0 || rj == 0 {
			return ri != 0
		}
		return ri > rj
	})
}

func normName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			space = false
		} else {
			space = true
		}
	}
	return b.String()
}

// matchSpeakers finds the speakers a spoken name means, best match only:
// the same name; else a name containing what was said (or the other way
// round); else one whose words start with each word said ("trib storm").
func matchSpeakers(query string, devs []btDevice) []btDevice {
	q := normName(query)
	if q == "" {
		return nil
	}
	tiers := [3][]btDevice{}
	for _, d := range devs {
		n := normName(d.Name)
		switch {
		case n == q:
			tiers[0] = append(tiers[0], d)
		case strings.Contains(n, q) || strings.Contains(q, n):
			tiers[1] = append(tiers[1], d)
		case wordsPrefix(q, n):
			tiers[2] = append(tiers[2], d)
		}
	}
	for _, t := range tiers {
		if len(t) > 0 {
			return t
		}
	}
	return nil
}

func wordsPrefix(q, n string) bool {
	words := strings.Fields(n)
	for _, w := range strings.Fields(q) {
		ok := false
		for _, nw := range words {
			if strings.HasPrefix(nw, w) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func sameName(devs []btDevice) bool {
	for _, d := range devs[1:] {
		if normName(d.Name) != normName(devs[0].Name) {
			return false
		}
	}
	return true
}

// pick chooses among speakers of the same name: a connected one, then a
// paired one, then the strongest signal (the closer one).
func pick(devs []btDevice) btDevice {
	best := devs[0]
	rank := func(d btDevice) int {
		switch {
		case d.Connected:
			return 2
		case d.Paired:
			return 1
		}
		return 0
	}
	for _, d := range devs[1:] {
		if rank(d) > rank(best) || (rank(d) == rank(best) && d.RSSI != 0 && (best.RSSI == 0 || d.RSSI > best.RSSI)) {
			best = d
		}
	}
	return best
}
