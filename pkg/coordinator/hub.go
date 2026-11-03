package coordinator

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"nexus/pkg/protocol"
	"nexus/pkg/resolver"
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
			// mpv plays the hub's stream URL, or resolves a video id itself;
			// the native player takes the hub's URL (it can't use yt-dlp).
			"player": map[string]any{"url": true, "resolves": c.cfg.Player != "native", "queue_copy": false},
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
		c.hubVolumeReport(true)
		c.volumeWatch.Do(func() { go c.hubVolumeLoop() })
	case "music_volume":
		var m struct {
			Volume int `json:"volume"`
		}
		if json.Unmarshal(data, &m) == nil {
			c.log.Info("Hub: volume", "volume", m.Volume)
			if err := c.setVolume(m.Volume); err != nil {
				c.log.Warn("Could not set the volume", "err", err)
			}
			c.hubVolumeReport(true)
		}
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

// VolumeControl is the volume Orbit reports and the hub sets: the speaker's
// own (audio.SinkVolume) rather than the player's.
type VolumeControl interface {
	Get(ctx context.Context) (int, error)
	Set(ctx context.Context, vol int) error
}

// SetVolumeControl makes v the volume the hub sees and sets.
func (c *Coordinator) SetVolumeControl(v VolumeControl) { c.volume = v }

func (c *Coordinator) setVolume(vol int) error {
	if c.volume != nil {
		return c.volume.Set(c.ctx, vol)
	}
	return c.player.SetVolume(vol)
}

func (c *Coordinator) getVolume() (int, bool) {
	if c.volume != nil {
		v, err := c.volume.Get(c.ctx)
		return v, err == nil
	}
	st, err := c.player.GetStatus()
	if err != nil {
		return 0, false
	}
	return st.Volume, true
}

// hubVolumeReport sends volume_state when the volume changed (or always,
// with force: after joining, or after the hub set it).
func (c *Coordinator) hubVolumeReport(force bool) {
	v, ok := c.getVolume()
	if !ok {
		return
	}
	c.stateMu.Lock()
	changed := force || v != c.lastVolume
	c.lastVolume = v
	c.stateMu.Unlock()
	if changed {
		c.sendJSON(map[string]any{"type": "volume_state", "volume": v})
	}
}

// hubVolumeLoop reports volume changes made elsewhere (the speaker's
// buttons, another app) every few seconds.
func (c *Coordinator) hubVolumeLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			c.hubVolumeReport(false)
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
