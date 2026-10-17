package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"nexus/pkg/store"
)

// Music is the hub's music controller (docs/PROTOCOL.md section 5): the
// queue, lookups, and which player plays. Players (edges with the player
// role) play what it sends and report back; controllers send actions and
// get music_state. One player plays at a time.
//
// Lookups take seconds, so they run without the lock; gen numbers each
// user change of track, and a lookup whose gen is stale drops its result.

const (
	musicLookupTimeout = 20 * time.Second
	musicRecs          = 8
	musicQueueLow      = 2  // refill from recommendations at or below this
	musicQueueSent     = 20 // entries sent in music_queue and music_state
	musicHistoryMax    = 50
	musicMaxSkips      = 6 // consecutive unplayable tracks before stopping
	musicURLMaxAge     = 5 * time.Hour
	musicRestartMs     = 3000 // "previous" after this restarts the song
)

var musicToolNames = map[string]bool{"play": true, "playback": true, "music_output": true}

type urlEntry struct {
	url string
	at  time.Time
}

type Music struct {
	h   *Hub
	src TrackSource
	log *slog.Logger

	mu         sync.Mutex
	output     string // the playing player's edge id; "" = none
	cur        *Track
	state      string // idle, resolving, buffering, playing, paused
	posMs      int64
	durMs      int64
	seq        uint64 // of the music_play the player is on; 0 = its own (music_sync)
	lastSeq    uint64
	gen        uint64
	queue      []Track
	history    []Track
	repeat     string // off, one, all
	autoplay   bool
	skips      int
	urls       map[string]urlEntry // fetched ahead, by video id
	fetching   map[string]bool
	refill     bool // a recommendations lookup is running
	refillPlay bool // ... and the queue ran out: play when it's done

	sessions map[string]bool // edges with a voice session on
	held     bool            // the hub holds the output paused for a session elsewhere
	// The music was playing when the sessions began (and nobody paused it
	// since): it plays again when they are over, wherever it is by then. A
	// player that has the session pauses and resumes its own music.
	sessionPaused bool
	boardHist     *boardPlaybackHistory
}

type boardPlaybackHistory struct {
	seq       uint64
	track     Track
	playerID  string
	ownerID   int64
	startedAt time.Time
	recorded  bool
}

func newMusic(h *Hub, src TrackSource) *Music {
	return &Music{h: h, src: src, log: h.log.With("component", "music"), state: "idle", repeat: "off",
		autoplay: true, urls: map[string]urlEntry{}, fetching: map[string]bool{}, sessions: map[string]bool{}}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edges

func (m *Music) edgeJoined(e *edge) {
	if e.roles.Controller {
		m.mu.Lock()
		msg := m.stateMsgLocked()
		m.mu.Unlock()
		m.sendTo(e, msg)
	}
}

func (m *Music) edgeLeft(e *edge) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[e.id] {
		m.voiceActiveLocked(e.id, false)
	}
	if e.id != m.output {
		m.broadcastLocked()
		return
	}
	if e.roles.Player != nil && e.roles.Player.QueueCopy {
		// It carries on alone and says where it is with music_sync when it
		// comes back.
		m.log.Info("Player left; it carries on with its queue copy", "player", e.id)
	} else {
		m.log.Info("Player left; its music stops", "player", e.id)
		m.output = ""
		m.state = "idle"
		m.gen++
	}
	m.broadcastLocked()
}

func (m *Music) fromEdge(e *edge, typ string, data []byte) {
	switch typ {
	case "voice_activity":
		var msg struct {
			Active bool `json:"active"`
		}
		json.Unmarshal(data, &msg)
		m.voiceActive(e.id, msg.Active)
	case "music_action":
		if !e.roles.Controller {
			return
		}
		var a musicAction
		if err := json.Unmarshal(data, &a); err != nil {
			return
		}
		go m.action(e, a)
	case "music_report":
		if e.roles.Player != nil {
			var r musicReport
			if json.Unmarshal(data, &r) == nil {
				m.report(e, r)
			}
		}
	case "music_event":
		if e.roles.Player != nil {
			var ev struct {
				Seq    uint64 `json:"seq"`
				ID     string `json:"id"`
				Event  string `json:"event"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(data, &ev) == nil {
				m.event(e, ev.Seq, ev.ID, ev.Event, ev.Reason)
			}
		}
	case "music_renew":
		var r struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &r) == nil && r.ID != "" && e.roles.Player != nil {
			go m.renew(e, r.ID)
		}
	case "music_sync":
		if e.roles.Player != nil {
			var s musicSync
			if json.Unmarshal(data, &s) == nil {
				m.sync(e, s)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Actions (controllers, and the voice tools)

type musicAction struct {
	Action     string `json:"action"`
	Query      string `json:"query"`
	ID         string `json:"id"`
	Cached     bool   `json:"cached"`
	PositionMs int64  `json:"position_ms"`
	Index      int    `json:"index"`
	Mode       string `json:"mode"`
	On         bool   `json:"on"`
	Target     string `json:"target"`
}

func (m *Music) action(from *edge, a musicAction) {
	ctx, cancel := context.WithTimeout(context.Background(), musicLookupTimeout)
	defer cancel()
	var err error
	switch a.Action {
	case "play":
		_, err = m.play(ctx, from, a.ID, a.Query, a.Cached)
	case "play_next", "queue":
		_, err = m.enqueue(ctx, a.ID, a.Query, a.Action == "play_next")
	case "next":
		m.next(true)
	case "previous":
		m.previous()
	case "pause", "resume", "toggle", "stop", "replay":
		m.control(a.Action)
	case "seek":
		m.seek(a.PositionMs)
	case "remove", "clear", "shuffle", "repeat", "autoplay":
		m.edit(a)
	case "output":
		_, err = m.selectOutput(from, a.Target)
	default:
		err = fmt.Errorf("unknown action %q", a.Action)
	}
	if err != nil {
		m.log.Warn("Music action failed", "action", a.Action, "from", from.id, "err", err)
	}
}

// play looks up id (or the top result for query) and plays it now on the
// output, clearing the queue. cached: the edge asking has the song saved,
// so no stream URL is needed if it plays there.
func (m *Music) play(ctx context.Context, from *edge, id, query string, cached bool) (Track, error) {
	m.mu.Lock()
	out := m.pickOutputLocked(from)
	if out == nil {
		m.mu.Unlock()
		return Track{}, errors.New("no player is connected")
	}
	m.gen++
	gen := m.gen
	prev := m.state
	m.state = "resolving"
	m.broadcastLocked()
	m.mu.Unlock()

	needURL := m.needsURL(out) && !(cached && out == from) && !m.h.cfg.Cache.Has(id)
	info, err := m.src.Lookup(ctx, id, query, needURL, musicRecs)
	if err == nil {
		m.h.cfg.Cache.Fetch(info.ID, info.URL)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen {
		return info.Track, errors.New("superseded")
	}
	if err != nil {
		m.state = prev
		m.broadcastLocked()
		return Track{}, err
	}
	m.output = out.id
	m.queue = nil
	m.pushHistoryLocked()
	m.startLocked(info.Track, info.URL, 0, false)
	if m.autoplay {
		m.addRecsLocked(info.Recs)
	}
	m.afterChangeLocked()
	return info.Track, nil
}

// enqueue adds a song after the current one (front) or at the end.
func (m *Music) enqueue(ctx context.Context, id, query string, front bool) (Track, error) {
	var t Track
	if id != "" {
		info, err := m.src.Lookup(ctx, id, "", false, 0)
		if err != nil {
			return Track{}, err
		}
		t = info.Track
	} else {
		found, err := m.src.Search(ctx, query, 1)
		if err != nil {
			return Track{}, err
		}
		t = found[0]
	}
	m.mu.Lock()
	if m.cur == nil || m.edgeLocked(m.output) == nil || m.state == "idle" {
		// Nothing plays: play it.
		m.mu.Unlock()
		return m.play(ctx, nil, t.ID, "", false)
	}
	defer m.mu.Unlock()
	if front {
		m.queue = append([]Track{t}, m.queue...)
	} else {
		m.queue = append(m.queue, t)
	}
	m.afterChangeLocked()
	return t, nil
}

// next moves to the next song. manual: asked for (skip); otherwise the song
// ended, and repeat applies.
func (m *Music) next(manual bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextLocked(manual)
}

func (m *Music) nextLocked(manual bool) {
	out := m.edgeLocked(m.output)
	if out == nil {
		out = m.pickOutputLocked(nil)
		if out == nil {
			return
		}
		m.output = out.id
	}
	if !manual && m.repeat == "one" && m.cur != nil {
		m.startTrackLocked(out, *m.cur)
		return
	}
	if m.repeat == "all" && m.cur != nil {
		m.queue = append(m.queue, *m.cur)
	}
	if len(m.queue) == 0 {
		if m.autoplay && m.cur != nil {
			// Out of songs: related ones to the last, then play the first.
			m.refillLocked(true)
			return
		}
		m.log.Info("Queue ended")
		m.gen++
		m.state = "idle"
		m.sendLocked(out, map[string]any{"type": "music_stop"})
		m.broadcastLocked()
		return
	}
	t := m.queue[0]
	m.queue = m.queue[1:]
	m.pushHistoryLocked()
	m.startTrackLocked(out, t)
}

func (m *Music) previous() {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.edgeLocked(m.output)
	if out == nil {
		return
	}
	if (m.cur != nil && m.posMs > musicRestartMs) || len(m.history) == 0 {
		m.sendLocked(out, map[string]any{"type": "music_seek", "position_ms": 0})
		m.posMs = 0
		m.broadcastLocked()
		return
	}
	t := m.history[len(m.history)-1]
	m.history = m.history[:len(m.history)-1]
	if m.cur != nil {
		m.queue = append([]Track{*m.cur}, m.queue...)
	}
	m.startTrackLocked(out, t)
}

func (m *Music) control(what string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.edgeLocked(m.output)
	if what == "toggle" {
		what = "pause"
		if m.state == "paused" || m.state == "idle" {
			what = "resume"
		}
	}
	switch what {
	case "pause":
		m.held = false
		m.sessionPaused = false
		if out != nil {
			m.sendLocked(out, map[string]any{"type": "music_pause"})
			m.state = "paused"
		}
	case "resume":
		if m.cur == nil {
			return
		}
		if out == nil {
			// Its player left: carry on where it was, on another.
			if out = m.pickOutputLocked(nil); out == nil {
				return
			}
			m.output = out.id
			m.startAtLocked(out, *m.cur, m.posMs, false)
			return
		}
		if m.state == "idle" {
			// Stopped (or the queue ended): play the song again.
			m.startTrackLocked(out, *m.cur)
			return
		}
		if m.sessionElsewhereLocked() {
			m.held = true // after the session
			m.sessionPaused = true
		} else {
			m.sendLocked(out, map[string]any{"type": "music_resume"})
			m.state = "playing"
		}
	case "replay":
		if out != nil && m.cur != nil {
			m.sendLocked(out, map[string]any{"type": "music_seek", "position_ms": 0})
			m.posMs = 0
		}
	case "stop":
		m.gen++
		m.queue = nil
		m.cur = nil
		m.state = "idle"
		m.held = false
		m.sessionPaused = false
		if out != nil {
			m.sendLocked(out, map[string]any{"type": "music_stop"})
		}
		m.sendQueueLocked()
	}
	m.broadcastLocked()
}

func (m *Music) seek(ms int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if out := m.edgeLocked(m.output); out != nil && m.cur != nil {
		m.sendLocked(out, map[string]any{"type": "music_seek", "position_ms": max(ms, 0)})
		m.posMs = max(ms, 0)
		m.broadcastLocked()
	}
}

func (m *Music) edit(a musicAction) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch a.Action {
	case "remove":
		if a.Index < 0 || a.Index >= len(m.queue) {
			return
		}
		m.queue = append(m.queue[:a.Index], m.queue[a.Index+1:]...)
	case "clear":
		m.queue = nil
	case "shuffle":
		rand.Shuffle(len(m.queue), func(i, j int) { m.queue[i], m.queue[j] = m.queue[j], m.queue[i] })
	case "repeat":
		if a.Mode != "off" && a.Mode != "one" && a.Mode != "all" {
			return
		}
		m.repeat = a.Mode
	case "autoplay":
		m.autoplay = a.On
	}
	m.afterChangeLocked()
}

// selectOutput moves the music to the player matching target (id, or a
// name, in part, case-insensitive; "board", "here" or "local" is the asking
// edge or the board). Returns the chosen player's name.
func (m *Music) selectOutput(from *edge, target string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	to := m.matchPlayerLocked(from, target)
	if to == nil {
		return "", fmt.Errorf("no player matches %q", target)
	}
	if to.id == m.output {
		return to.name, nil
	}
	old := m.edgeLocked(m.output)
	m.output = to.id
	m.log.Info("Music output", "to", to.id)
	if m.cur == nil || m.state == "idle" {
		m.broadcastLocked()
		return to.name, nil
	}
	if old != nil {
		m.sendLocked(old, map[string]any{"type": "music_stop"})
	}
	// Paused by the person: stays paused. A session on another edge than
	// the new output: held paused until it ends.
	userPaused := m.state == "paused" && !m.sessionPaused
	m.held = m.sessionElsewhereLocked() && !userPaused
	m.startAtLocked(to, *m.cur, m.posMs, userPaused || m.held)
	return to.name, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// From players

type musicReport struct {
	Seq        uint64 `json:"seq"`
	ID         string `json:"id"`
	State      string `json:"state"`
	PositionMs int64  `json:"position_ms"`
	DurationMs int64  `json:"duration_ms"`
}

func (m *Music) report(e *edge, r musicReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.id != m.output || r.Seq != m.seq || m.cur == nil || (r.ID != "" && r.ID != m.cur.ID) {
		return
	}
	switch r.State {
	case "buffering", "playing", "paused", "idle":
		m.state = r.State
	}
	m.posMs = r.PositionMs
	if r.DurationMs > 0 {
		m.durMs = r.DurationMs
	}
	if r.State == "playing" {
		m.skips = 0
	}
	if m.boardHist != nil && !m.boardHist.recorded && m.boardHist.seq == r.Seq && m.boardHist.playerID == e.id {
		if r.PositionMs >= 30000 || time.Since(m.boardHist.startedAt) >= 30*time.Second {
			m.boardHist.recorded = true
			bh := *m.boardHist
			go m.recordBoardHistory(bh, r.PositionMs)
		}
	}
	m.broadcastLocked()
}

func (m *Music) event(e *edge, seq uint64, id, ev, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.id != m.output || seq != m.seq || m.cur == nil || (id != "" && id != m.cur.ID) {
		return
	}
	switch ev {
	case "ended":
		if m.boardHist != nil && !m.boardHist.recorded && m.boardHist.seq == seq && m.boardHist.playerID == e.id {
			m.boardHist.recorded = true
			bh := *m.boardHist
			playedMs := m.durMs
			if m.posMs > 0 && (playedMs <= 0 || m.posMs > playedMs) {
				playedMs = m.posMs
			}
			go m.recordBoardHistory(bh, playedMs)
		}
		m.nextLocked(false)
	case "error":
		m.skips++
		m.log.Warn("Player error", "player", e.id, "id", id, "reason", reason, "skips", m.skips)
		delete(m.urls, m.cur.ID)
		if m.skips > musicMaxSkips {
			m.log.Warn("Too many unplayable songs; stopping")
			m.gen++
			m.state = "idle"
			m.skips = 0
			m.broadcastLocked()
			return
		}
		m.nextLocked(true)
	}
}

func (m *Music) renew(e *edge, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), musicLookupTimeout)
	defer cancel()
	info, err := m.src.Lookup(ctx, id, "", true, 0)
	if err != nil {
		m.log.Warn("Stream URL renewal failed", "id", id, "err", err)
		m.sendTo(e, map[string]any{"type": "music_url", "id": id, "error": errReason(err)})
		return
	}
	m.mu.Lock()
	m.urls[id] = urlEntry{info.URL, time.Now()}
	m.mu.Unlock()
	m.sendTo(e, map[string]any{"type": "music_url", "id": id, "url": info.URL})
}

type musicSync struct {
	Track      *Track  `json:"track"`
	State      string  `json:"state"`
	PositionMs int64   `json:"position_ms"`
	Queue      []Track `json:"queue"`
}

// sync: a player that played on its own while the hub was away (or before
// it joined) says where it is; that becomes the music state.
func (m *Music) sync(e *edge, s musicSync) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.Track == nil || s.Track.ID == "" || s.State == "idle" {
		if e.id == m.output {
			// It stopped while away.
			m.state = "idle"
			m.broadcastLocked()
		}
		return
	}
	m.log.Info("Player synced its music", "player", e.id, "id", s.Track.ID, "state", s.State, "queue", len(s.Queue))
	m.gen++
	m.output = e.id
	t := *s.Track
	m.cur = &t
	m.state = s.State
	m.posMs = s.PositionMs
	m.durMs = int64(t.Duration) * 1000
	m.seq = 0
	m.queue = append([]Track(nil), s.Queue...)
	m.afterChangeLocked()
}

// ─────────────────────────────────────────────────────────────────────────────
// Voice sessions: music pauses wherever it plays

func (m *Music) voiceActive(edgeID string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.voiceActiveLocked(edgeID, on)
}

func (m *Music) voiceActiveLocked(edgeID string, on bool) {
	if on == m.sessions[edgeID] {
		return
	}
	first := on && len(m.sessions) == 0
	if on {
		m.sessions[edgeID] = true
	} else {
		delete(m.sessions, edgeID)
	}
	playing := m.state == "playing" || m.state == "buffering" || m.state == "resolving"
	if first && playing && m.cur != nil {
		m.sessionPaused = true
	}
	out := m.edgeLocked(m.output)
	// A player pauses its own music for its own session; another's, the hub
	// pauses.
	if on && out != nil && edgeID != m.output && playing && !m.held {
		m.sendLocked(out, map[string]any{"type": "music_pause"})
		m.state = "paused"
		m.held = true
		m.broadcastLocked()
	}
	if len(m.sessions) > 0 {
		return
	}
	// All sessions are over.
	if m.held && out != nil {
		m.sendLocked(out, map[string]any{"type": "music_resume"})
		m.state = "playing"
		m.broadcastLocked()
	}
	m.held = false
	m.sessionPaused = false
}

func (m *Music) sessionElsewhereLocked() bool {
	for id := range m.sessions {
		if id != m.output {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// Voice tools (the hub owns them: docs/PROTOCOL.md section 4)

func musicToolDeclarations() []map[string]any {
	str := func(desc string, enum ...string) map[string]any {
		p := map[string]any{"type": "STRING", "description": desc}
		if len(enum) > 0 {
			p["enum"] = enum
		}
		return p
	}
	return []map[string]any{
		{"name": "play", "description": "Plays a song: now (clears the queue), or next (right after the current track). Pass what the user asked for as the query.",
			"parameters": map[string]any{"type": "OBJECT", "required": []string{"query"}, "properties": map[string]any{
				"query": str("Song title and/or artist, or a genre, e.g. 'Blinding Lights'."),
				"when":  str("'now' (default) or 'next'.", "now", "next")}}},
		{"name": "playback", "description": "Controls music playback. 'stop' also clears the queue; 'resume' is for 'play' or 'resume' without a song name.",
			"parameters": map[string]any{"type": "OBJECT", "required": []string{"action"}, "properties": map[string]any{
				"action": str("pause, resume, stop, next or previous.", "pause", "resume", "stop", "next", "previous")}}},
		{"name": "music_output", "description": "Moves the music to another speaker, carrying on the current song from where it is: this device ('here'), or another device by its name (a PC, a Raspberry Pi, a phone). Use for 'play it on my PC' or 'bring the music back here'. Without a target it lists the speakers and says where the music plays now. When only one other speaker is connected, any name picks it.",
			"parameters": map[string]any{"type": "OBJECT", "properties": map[string]any{
				"target": str("'here', or a speaker's name; omit to list them.")}}},
	}
}

// toolCall runs one of the music tools for a voice session on edge from.
func (m *Music) toolCall(from *edge, name string, args json.RawMessage) json.RawMessage {
	var a struct {
		Query  string `json:"query"`
		When   string `json:"when"`
		Action string `json:"action"`
		Target string `json:"target"`
	}
	json.Unmarshal(args, &a)
	ctx, cancel := context.WithTimeout(context.Background(), musicLookupTimeout)
	defer cancel()
	switch name {
	case "play":
		if strings.TrimSpace(a.Query) == "" {
			return toolError("query is required")
		}
		var t Track
		var err error
		if a.When == "next" {
			t, err = m.enqueue(ctx, "", a.Query, true)
		} else {
			t, err = m.play(ctx, from, "", a.Query, false)
		}
		if err != nil {
			if errors.Is(err, ErrTrackNotFound) {
				return toolError("nothing found for that")
			}
			return toolError("music lookup failed")
		}
		key := "playing"
		if a.When == "next" {
			key = "queued_next"
		}
		return mustJSON(map[string]any{"status": "success", key: t.Title + " by " + t.Artist})
	case "playback":
		switch a.Action {
		case "pause", "resume", "stop":
			m.control(a.Action)
		case "next":
			m.next(true)
		case "previous":
			m.previous()
		default:
			return toolError("action must be pause, resume, stop, next or previous")
		}
		return mustJSON(map[string]any{"status": "success"})
	case "music_output":
		if a.Target != "" {
			chosen, err := m.selectOutput(from, a.Target)
			if err == nil {
				return mustJSON(map[string]any{"status": "success", "now_playing_on": chosen})
			}
		}
		m.mu.Lock()
		res := map[string]any{"speakers": m.playerNamesLocked(), "now_playing_on": "nothing"}
		if out := m.edgeLocked(m.output); out != nil {
			res["now_playing_on"] = out.name
		}
		m.mu.Unlock()
		res["status"] = "success"
		if a.Target != "" {
			res["status"] = "error"
			res["message"] = "no speaker by that name"
		}
		return mustJSON(res)
	}
	return toolError("unknown tool")
}

// ─────────────────────────────────────────────────────────────────────────────
// Internals (m.mu held)

// startTrackLocked plays t on out from the start, with a stream URL fetched
// ahead if there is one, else after a lookup.
func (m *Music) startTrackLocked(out *edge, t Track) {
	m.startAtLocked(out, t, 0, false)
}

func (m *Music) startAtLocked(out *edge, t Track, posMs int64, paused bool) {
	m.gen++
	url := ""
	if m.needsURL(out) && !m.h.cfg.Cache.Has(t.ID) {
		if u, ok := m.urls[t.ID]; ok && time.Since(u.at) < musicURLMaxAge {
			url = u.url
		} else {
			// Look it up, then start (unless something else was asked meanwhile).
			gen := m.gen
			m.cur = &t
			m.state = "resolving"
			m.broadcastLocked()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), musicLookupTimeout)
				defer cancel()
				info, err := m.src.Lookup(ctx, t.ID, "", true, 0)
				m.mu.Lock()
				defer m.mu.Unlock()
				if gen != m.gen {
					return
				}
				if err != nil {
					m.log.Warn("Lookup failed; skipping", "id", t.ID, "err", err)
					m.skips++
					if m.skips > musicMaxSkips {
						m.state = "idle"
						m.skips = 0
						m.broadcastLocked()
						return
					}
					m.nextLocked(true)
					return
				}
				m.h.cfg.Cache.Fetch(t.ID, info.URL)
				m.startLocked(t, info.URL, posMs, paused)
				m.afterChangeLocked()
			}()
			return
		}
	}
	m.startLocked(t, url, posMs, paused)
	m.afterChangeLocked()
}

// startLocked sends music_play to the output.
func (m *Music) startLocked(t Track, url string, posMs int64, paused bool) {
	out := m.edgeLocked(m.output)
	if out == nil {
		return
	}
	m.lastSeq++
	m.seq = m.lastSeq
	m.cur = &t
	m.posMs = posMs
	m.durMs = int64(t.Duration) * 1000
	m.state = "buffering"
	if paused {
		m.state = "paused"
	}
	delete(m.urls, t.ID) // a URL is used once; renewals ask again
	if m.h.cfg.Cache.Has(t.ID) {
		url = m.h.cfg.Cache.URL(out.base, t.ID) // from the hub, over the LAN
	}
	msg := map[string]any{"type": "music_play", "track": t, "position_ms": posMs, "paused": paused, "seq": m.seq}
	if url != "" {
		msg["url"] = url
	}
	if out.ownerID > 0 {
		m.boardHist = &boardPlaybackHistory{
			seq:       m.seq,
			track:     t,
			playerID:  out.id,
			ownerID:   out.ownerID,
			startedAt: time.Now(),
			recorded:  false,
		}
	} else {
		m.boardHist = nil
	}
	m.log.Info("Playing", "id", t.ID, "title", t.Title, "player", out.id, "seq", m.seq)
	m.sendLocked(out, msg)
}

// afterChangeLocked: the queue or the song changed. Send the queue copy and
// the state, refill from recommendations, fetch the next song's URL.
func (m *Music) afterChangeLocked() {
	m.sendQueueLocked()
	m.broadcastLocked()
	if m.autoplay && m.cur != nil && len(m.queue) <= musicQueueLow {
		m.refillLocked(false)
	}
	m.prefetchLocked()
}

// refillLocked adds songs related to the current one. thenPlay: the queue
// ran out; play the first when they arrive.
func (m *Music) refillLocked(thenPlay bool) {
	if m.cur == nil {
		return
	}
	if m.refill {
		m.refillPlay = m.refillPlay || thenPlay
		return
	}
	m.refill = true
	m.refillPlay = thenPlay
	base := *m.cur
	gen := m.gen
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), musicLookupTimeout)
		defer cancel()
		info, err := m.src.Lookup(ctx, base.ID, "", false, musicRecs)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.refill = false
		thenPlay := m.refillPlay
		m.refillPlay = false
		if err != nil {
			m.log.Warn("Recommendations failed", "id", base.ID, "err", err)
			if thenPlay && gen == m.gen {
				m.state = "idle"
				m.broadcastLocked()
			}
			return
		}
		m.addRecsLocked(info.Recs)
		if thenPlay {
			if gen == m.gen && len(m.queue) > 0 {
				m.nextLocked(true)
			}
			return
		}
		m.sendQueueLocked()
		m.broadcastLocked()
		m.prefetchLocked()
	}()
}

func (m *Music) addRecsLocked(recs []Track) {
	seen := map[string]bool{}
	if m.cur != nil {
		seen[m.cur.ID] = true
	}
	for _, t := range m.queue {
		seen[t.ID] = true
	}
	for _, t := range m.history {
		seen[t.ID] = true
	}
	for _, t := range recs {
		if !seen[t.ID] && t.ID != "" {
			m.queue = append(m.queue, t)
			seen[t.ID] = true
		}
	}
}

// prefetchLocked looks up the next song's stream URL, so a track change is
// one message to the player.
func (m *Music) prefetchLocked() {
	out := m.edgeLocked(m.output)
	if out == nil || len(m.queue) == 0 || !(m.needsURL(out) || m.h.cfg.Cache != nil) {
		return
	}
	id := m.queue[0].ID
	if u, ok := m.urls[id]; (ok && time.Since(u.at) < musicURLMaxAge) || m.fetching[id] || m.h.cfg.Cache.Has(id) {
		return
	}
	m.fetching[id] = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), musicLookupTimeout)
		defer cancel()
		info, err := m.src.Lookup(ctx, id, "", true, 0)
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.fetching, id)
		if err == nil {
			m.urls[id] = urlEntry{info.URL, time.Now()}
			m.h.cfg.Cache.Fetch(id, info.URL)
		}
	}()
}

func (m *Music) pushHistoryLocked() {
	if m.cur == nil {
		return
	}
	m.history = append(m.history, *m.cur)
	if len(m.history) > musicHistoryMax {
		m.history = m.history[len(m.history)-musicHistoryMax:]
	}
}

// needsURL: the player can only play a stream URL (the board).
func (m *Music) needsURL(e *edge) bool {
	p := e.roles.Player
	return p != nil && !p.Resolves
}

func (m *Music) edgeLocked(id string) *edge {
	if id == "" {
		return nil
	}
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	e := m.h.edges[id]
	if e == nil || e.roles.Player == nil {
		return nil
	}
	return e
}

// pickOutputLocked: the current output if connected, else the asking edge
// if it plays, else the board, else any player.
func (m *Music) pickOutputLocked(from *edge) *edge {
	if e := m.edgeLocked(m.output); e != nil {
		return e
	}
	if from != nil && from.roles.Player != nil {
		return from
	}
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	if b := m.h.board; b != nil && b.roles.Player != nil {
		return b
	}
	var pick *edge
	for _, e := range m.h.edges {
		if e.roles.Player != nil && (pick == nil || e.since.Before(pick.since)) {
			pick = e
		}
	}
	return pick
}

func (m *Music) matchPlayerLocked(from *edge, target string) *edge {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return nil
	}
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	if t == "here" || t == "local" || t == "board" || t == "this device" {
		if from != nil && from.roles.Player != nil {
			return from
		}
		if b := m.h.board; b != nil && b.roles.Player != nil {
			return b
		}
	}
	var players, others []*edge
	for _, e := range m.h.edges {
		if e.roles.Player == nil {
			continue
		}
		players = append(players, e)
		if from == nil || e.id != from.id {
			others = append(others, e)
		}
	}
	for _, e := range players {
		if strings.ToLower(e.id) == t || strings.ToLower(e.name) == t {
			return e
		}
	}
	for _, e := range players {
		if strings.Contains(strings.ToLower(e.name), t) {
			return e
		}
	}
	// One other speaker: any name picks it (as the board's tool does).
	if len(others) == 1 {
		return others[0]
	}
	return nil
}

func (m *Music) playerNamesLocked() []string {
	m.h.mu.Lock()
	defer m.h.mu.Unlock()
	var names []string
	for _, e := range m.h.edges {
		if e.roles.Player != nil {
			names = append(names, e.name)
		}
	}
	return names
}

func (m *Music) sendQueueLocked() {
	out := m.edgeLocked(m.output)
	if out == nil || !out.roles.Player.QueueCopy {
		return
	}
	m.sendLocked(out, map[string]any{"type": "music_queue", "tracks": m.queueHeadLocked(),
		"repeat": m.repeat, "autoplay": m.autoplay})
}

func (m *Music) queueHeadLocked() []Track {
	q := m.queue
	if len(q) > musicQueueSent {
		q = q[:musicQueueSent]
	}
	return append([]Track{}, q...)
}

func (m *Music) stateMsgLocked() map[string]any {
	msg := map[string]any{"type": "music_state", "output": m.output, "state": m.state,
		"position_ms": m.posMs, "duration_ms": m.durMs, "queue": m.queueHeadLocked(),
		"repeat": m.repeat, "autoplay": m.autoplay}
	if m.cur != nil {
		msg["track"] = m.cur
	}
	m.h.mu.Lock()
	var players []map[string]any
	for _, e := range m.h.edges {
		if e.roles.Player != nil {
			players = append(players, map[string]any{"id": e.id, "name": e.name, "kind": e.kind})
		}
	}
	m.h.mu.Unlock()
	msg["players"] = players
	return msg
}

// broadcastLocked sends music_state to every controller.
func (m *Music) broadcastLocked() {
	data := mustJSON(m.stateMsgLocked())
	m.h.mu.Lock()
	var targets []*edge
	for _, e := range m.h.edges {
		if e.roles.Controller {
			targets = append(targets, e)
		}
	}
	m.h.mu.Unlock()
	for _, e := range targets {
		m.sendRaw(e, data)
	}
}

func (m *Music) sendLocked(e *edge, msg any) { m.sendRaw(e, mustJSON(msg)) }

func (m *Music) sendTo(e *edge, msg any) { m.sendRaw(e, mustJSON(msg)) }

func (m *Music) sendRaw(e *edge, data []byte) {
	select {
	case e.send <- data:
	default:
		m.log.Warn("Edge send queue full; dropped a music message", "edge", e.id)
	}
}

func errReason(err error) string {
	if errors.Is(err, ErrTrackNotFound) {
		return "not_found"
	}
	return "lookup_failed"
}

func (m *Music) recordBoardHistory(bh boardPlaybackHistory, playedMs int64) {
	if m.h == nil || m.h.store == nil || bh.ownerID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var devID *string
	if bh.playerID != "" {
		if d, err := m.h.store.GetDeviceByID(ctx, bh.playerID); err == nil && d != nil {
			devID = &bh.playerID
		}
	}
	entry := store.HistoryEntry{
		Track:    bh.track.toStore(),
		PlayedAt: bh.startedAt.UnixMilli(),
		PlayedMs: playedMs,
		DeviceID: devID,
		Source:   "board",
	}
	if _, err := m.h.store.AddHistory(ctx, bh.ownerID, []store.HistoryEntry{entry}); err != nil {
		m.log.Warn("Failed to record board history", "err", err, "track", bh.track.ID, "user", bh.ownerID)
	} else {
		m.log.Info("Recorded board history", "track", bh.track.ID, "user", bh.ownerID, "played_ms", playedMs)
	}
}
