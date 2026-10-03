package coordinator

import (
	"encoding/json"
	"strings"

	"nexus-orbit/pkg/protocol"
	"nexus-orbit/pkg/resolver"
)

// Hub mode (--hub): Orbit is an edge of nexus-hub with the player role
// (docs/PROTOCOL.md section 5). The hub owns the queue: it sends music_play
// (with a stream URL, or the id alone, which Orbit resolves), pause, resume,
// seek and stop; Orbit reports its state every second and says when a
// track ends. Without --hub it speaks the board's /api/orbit/ws dialect.

const hubProto = 2

func (c *Coordinator) hubMode() bool { return c.cfg.HubURL != "" }

func (c *Coordinator) sendJSON(v any) {
	if c.transport == nil {
		return
	}
	if data, err := json.Marshal(v); err == nil {
		_ = c.transport.Send(data)
	}
}

func (c *Coordinator) hubHello() {
	c.log.Info("Joining the hub", "url", c.cfg.HubURL)
	c.sendJSON(map[string]any{
		"type": "hello", "proto": hubProto, "id": c.cfg.SatelliteID, "name": c.cfg.SatelliteName,
		"kind": "orbit", "fw": "nexus-orbit", "token": c.cfg.HubToken,
		"roles": map[string]any{
			// mpv plays the hub's stream URL, or resolves a video id itself.
			"player": map[string]any{"url": true, "resolves": true, "queue_copy": false},
		},
	})
}

func (c *Coordinator) handleHubMessage(data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return
	}
	switch head.Type {
	case "welcome":
		c.log.Info("Joined the hub", "hub", string(data))
	case "rejected":
		c.log.Error("The hub refused this satellite (token?)", "msg", string(data))
	case "music_play":
		var m struct {
			Track struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Artist   string `json:"artist"`
				Duration int    `json:"duration"`
			} `json:"track"`
			URL        string `json:"url"`
			PositionMs int64  `json:"position_ms"`
			Paused     bool   `json:"paused"`
			Seq        uint64 `json:"seq"`
		}
		if json.Unmarshal(data, &m) != nil || m.Track.ID == "" {
			return
		}
		c.log.Info("Hub: play", "videoId", m.Track.ID, "posMs", m.PositionMs, "seq", m.Seq, "url", m.URL != "")
		gen := c.beginPlay(m.Track.ID, m.PositionMs)
		c.stateMu.Lock()
		c.hubSeq = m.Seq
		c.wantPaused = m.Paused
		c.stateMu.Unlock()
		_ = c.player.Stop()
		if m.URL == "" {
			go func() { _ = c.playTrack(c.ctx, gen, m.Track.ID, m.PositionMs) }()
			return
		}
		go c.playResolved(gen, &resolver.ResolvedTrack{VideoID: m.Track.ID, PlaybackURL: m.URL,
			Title: m.Track.Title, Artist: m.Track.Artist, DurationSecs: m.Track.Duration}, m.PositionMs)
	case "music_pause":
		c.pause()
	case "music_resume":
		c.resume()
	case "music_stop":
		c.stop()
	case "music_seek":
		var m struct {
			PositionMs int64 `json:"position_ms"`
		}
		if json.Unmarshal(data, &m) == nil {
			_ = c.player.SeekTo(m.PositionMs)
		}
	}
}

func hubState(s protocol.PlaybackState) string {
	switch s {
	case protocol.StatePlaying:
		return "playing"
	case protocol.StatePaused:
		return "paused"
	case protocol.StateResolving, protocol.StateBuffering:
		return "buffering"
	default:
		return "idle"
	}
}

func (c *Coordinator) hubReport(vid string, st protocol.PlaybackState, posMs, durMs int64) {
	c.stateMu.RLock()
	seq := c.hubSeq
	c.stateMu.RUnlock()
	c.sendJSON(map[string]any{"type": "music_report", "seq": seq, "id": vid, "state": hubState(st),
		"position_ms": posMs, "duration_ms": durMs})
}

func (c *Coordinator) hubEvent(eventType, videoID, msg string) {
	c.stateMu.RLock()
	seq := c.hubSeq
	c.stateMu.RUnlock()
	ev := map[string]any{"type": "music_event", "seq": seq, "id": videoID}
	switch eventType {
	case "track_ended":
		ev["event"] = "ended"
	case "error":
		ev["event"] = "error"
		ev["reason"] = msg
		if strings.Contains(msg, "resolution failed") {
			ev["reason"] = "lookup: " + msg
		}
	default:
		return
	}
	c.sendJSON(ev)
}
