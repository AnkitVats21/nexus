package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"nexus-orbit/config"
	"nexus-orbit/pkg/audio"
	"nexus-orbit/pkg/desktop"
	"nexus-orbit/pkg/protocol"
	"nexus-orbit/pkg/resolver"
	"nexus-orbit/pkg/transport"
)

// Coordinator orchestrates Transport, Resolver, Audio Engine, and MPRIS.
type Coordinator struct {
	cfg       *config.Config
	transport transport.Transport
	resolver  resolver.Resolver
	player    audio.AudioPlayer
	mpris     *desktop.MPRISManager
	log       *slog.Logger

	stateMu      sync.RWMutex
	currentTrack *resolver.ResolvedTrack
	currentState protocol.PlaybackState

	ctx    context.Context
	cancel context.CancelFunc
}

// NewCoordinator creates a new Coordinator instance.
func NewCoordinator(
	cfg *config.Config,
	trans transport.Transport,
	res resolver.Resolver,
	player audio.AudioPlayer,
	mpris *desktop.MPRISManager,
) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Coordinator{
		cfg:          cfg,
		transport:    trans,
		resolver:     res,
		player:       player,
		mpris:        mpris,
		log:          slog.Default().With("component", "coordinator"),
		currentState: protocol.StateIdle,
		ctx:          ctx,
		cancel:       cancel,
	}

	c.setupCallbacks()
	return c
}

// Start begins processing inbound transport events and periodic reports.
func (c *Coordinator) Start() error {
	c.log.Info("Starting State Coordinator", "satelliteID", c.cfg.SatelliteID, "resolver", c.resolver.Name())

	if c.transport != nil {
		c.transport.OnConnected(c.handleTransportConnected)
		go c.transportReadLoop()
		go c.telemetryLoop()
	}

	return nil
}

// Stop shuts down the coordinator and releases resources.
func (c *Coordinator) Stop() {
	c.cancel()
	if c.player != nil {
		_ = c.player.Stop()
	}
	if c.mpris != nil {
		_ = c.mpris.Close()
	}
	if c.transport != nil {
		_ = c.transport.Close()
	}
}

// PlayTrack resolves and plays a YouTube video directly.
func (c *Coordinator) PlayTrack(ctx context.Context, videoID string, startPosMs int64) error {
	c.log.Info("PlayTrack requested", "videoID", videoID, "startPosMs", startPosMs)

	c.setLocalState(protocol.StateResolving)

	resolved, err := c.resolver.Resolve(ctx, videoID)
	if err != nil {
		c.setLocalState(protocol.StateError)
		c.sendEvent("error", videoID, fmt.Sprintf("resolution failed: %v", err))
		return fmt.Errorf("failed to resolve track %s: %w", videoID, err)
	}

	c.stateMu.Lock()
	c.currentTrack = resolved
	c.stateMu.Unlock()

	c.log.Info("Track resolved successfully",
		"videoID", resolved.VideoID,
		"title", resolved.Title,
		"playbackURL", resolved.PlaybackURL,
	)

	if err := c.player.LoadURL(resolved.PlaybackURL, startPosMs); err != nil {
		c.setLocalState(protocol.StateError)
		c.sendEvent("error", videoID, fmt.Sprintf("player load failed: %v", err))
		return fmt.Errorf("failed to load audio URL: %w", err)
	}

	c.setLocalState(protocol.StatePlaying)
	c.updateMPRIS("Playing")
	return nil
}

// ── Private Handlers ─────────────────────────────────────────────────────────

func (c *Coordinator) setupCallbacks() {
	// MPV track ended callback
	c.player.OnTrackEnded(func() {
		c.log.Info("Audio playback ended (EOF reached)")
		c.setLocalState(protocol.StateIdle)
		c.updateMPRIS("Stopped")

		c.stateMu.RLock()
		curID := ""
		if c.currentTrack != nil {
			curID = c.currentTrack.VideoID
		}
		c.stateMu.RUnlock()

		c.sendEvent("track_ended", curID, "")
		c.sendAction("next", nil) // Prompt Nexus to advance queue
	})

	// MPV status change callback
	c.player.OnStatusChanged(func(status audio.PlayerStatus) {
		c.setLocalState(status.State)
		switch status.State {
		case protocol.StatePlaying:
			c.updateMPRIS("Playing")
		case protocol.StatePaused:
			c.updateMPRIS("Paused")
		case protocol.StateStopped, protocol.StateIdle:
			c.updateMPRIS("Stopped")
		}
	})

	// MPRIS desktop media keys callbacks
	if c.mpris != nil {
		c.mpris.SetHandlers(
			func() { // PlayPause
				c.log.Debug("MPRIS PlayPause triggered")
				status, _ := c.player.GetStatus()
				if status.State == protocol.StatePlaying {
					_ = c.player.Pause()
				} else {
					_ = c.player.Resume()
				}
				c.sendAction("toggle", nil)
			},
			func() { // Play
				_ = c.player.Resume()
				c.sendAction("resume", nil)
			},
			func() { // Pause
				_ = c.player.Pause()
				c.sendAction("pause", nil)
			},
			func() { // Next
				c.sendAction("next", nil)
			},
			func() { // Previous
				c.sendAction("previous", nil)
			},
			func() { // Stop
				_ = c.player.Stop()
				c.sendAction("stop", nil)
			},
			func(posUsec int64) { // Seek
				posMs := posUsec / 1000
				_ = c.player.Seek(posMs)
				c.sendAction("seek", posMs/1000)
			},
		)
	}
}

func (c *Coordinator) handleTransportConnected() {
	c.log.Info("Registering satellite with Nexus Core")
	reg := protocol.SatelliteRegisterMsg{
		Cmd: "satellite_register",
		Satellite: protocol.SatelliteInfo{
			ID:           c.cfg.SatelliteID,
			Name:         c.cfg.SatelliteName,
			Type:         "desktop",
			Capabilities: []string{"audio_playback", "volume_control", "media_keys"},
		},
	}

	payload, err := json.Marshal(reg)
	if err == nil {
		_ = c.transport.Send(payload)
	}
}

func (c *Coordinator) transportReadLoop() {
	msgCh, errCh := c.transport.Receive()
	for {
		select {
		case <-c.ctx.Done():
			return
		case err := <-errCh:
			c.log.Warn("Transport error received", "err", err)
		case data := <-msgCh:
			c.handleInboundMessage(data)
		}
	}
}

func (c *Coordinator) handleInboundMessage(data []byte) {
	var base protocol.BaseMessage
	if err := json.Unmarshal(data, &base); err != nil {
		c.log.Debug("Unrecognized JSON payload", "raw", string(data))
		return
	}

	switch base.Cmd {
	case "orbit_route":
		var route protocol.OrbitRouteMsg
		if err := json.Unmarshal(data, &route); err == nil {
			c.log.Info("Received orbit_route command", "videoId", route.VideoID, "posMs", route.PositionMs)
			go func() {
				_ = c.PlayTrack(c.ctx, route.VideoID, route.PositionMs)
			}()
		}

	case "orbit_play":
		var play protocol.OrbitRouteMsg
		if err := json.Unmarshal(data, &play); err == nil {
			c.log.Info("Received orbit_play command", "videoId", play.VideoID)
			go func() {
				_ = c.PlayTrack(c.ctx, play.VideoID, 0)
			}()
		}

	case "orbit_pause":
		c.log.Info("Received orbit_pause command")
		_ = c.player.Pause()

	case "orbit_resume":
		c.log.Info("Received orbit_resume command")
		_ = c.player.Resume()

	case "orbit_seek":
		var seek protocol.OrbitSeekMsg
		if err := json.Unmarshal(data, &seek); err == nil {
			c.log.Info("Received orbit_seek command", "posMs", seek.PositionMs)
			_ = c.player.Seek(seek.PositionMs)
		}

	case "orbit_volume":
		var vol protocol.OrbitVolumeMsg
		if err := json.Unmarshal(data, &vol); err == nil {
			c.log.Info("Received orbit_volume command", "volume", vol.Volume)
			_ = c.player.SetVolume(vol.Volume)
		}

	case "orbit_stop":
		c.log.Info("Received orbit_stop command")
		_ = c.player.Stop()

	default:
		c.log.Debug("Ignored inbound command", "cmd", base.Cmd)
	}
}

func (c *Coordinator) telemetryLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.emitTelemetryReport()
		}
	}
}

func (c *Coordinator) emitTelemetryReport() {
	if c.transport == nil {
		return
	}

	status, err := c.player.GetStatus()
	if err != nil {
		return
	}

	c.stateMu.RLock()
	var vid string
	if c.currentTrack != nil {
		vid = c.currentTrack.VideoID
	}
	c.stateMu.RUnlock()

	rep := protocol.OrbitReportMsg{
		Cmd:         "orbit_report",
		SatelliteID: c.cfg.SatelliteID,
		VideoID:     vid,
		State:       status.State,
		PositionMs:  status.PositionMs,
		DurationMs:  status.DurationMs,
		Volume:      status.Volume,
	}

	data, err := json.Marshal(rep)
	if err == nil {
		_ = c.transport.Send(data)
	}
}

func (c *Coordinator) sendEvent(eventType, videoID, msg string) {
	if c.transport == nil {
		return
	}
	evt := protocol.OrbitEventMsg{
		Cmd:         "orbit_event",
		SatelliteID: c.cfg.SatelliteID,
		Event:       eventType,
		VideoID:     videoID,
		Message:     msg,
	}
	data, err := json.Marshal(evt)
	if err == nil {
		_ = c.transport.Send(data)
	}
}

func (c *Coordinator) sendAction(action string, val any) {
	if c.transport == nil {
		return
	}
	act := protocol.NexusActionMsg{
		Cmd:    "action",
		Action: action,
		Value:  val,
	}
	data, err := json.Marshal(act)
	if err == nil {
		_ = c.transport.Send(data)
	}
}

func (c *Coordinator) setLocalState(st protocol.PlaybackState) {
	c.stateMu.Lock()
	c.currentState = st
	c.stateMu.Unlock()
}

func (c *Coordinator) updateMPRIS(statusStr string) {
	if c.mpris == nil {
		return
	}

	c.stateMu.RLock()
	track := c.currentTrack
	c.stateMu.RUnlock()

	status, _ := c.player.GetStatus()

	meta := desktop.TrackMetadata{}
	if track != nil {
		meta.TrackID = track.VideoID
		meta.Title = track.Title
		if meta.Title == "" {
			meta.Title = fmt.Sprintf("YouTube Track (%s)", track.VideoID)
		}
		meta.Artist = track.Artist
		if meta.Artist == "" {
			meta.Artist = "Nexus Orbit"
		}
		meta.ArtURL = track.ThumbnailURL
		meta.DurationUsec = int64(track.DurationSecs) * 1_000_000
		if meta.DurationUsec == 0 && status.DurationMs > 0 {
			meta.DurationUsec = status.DurationMs * 1000
		}
	}

	vol := float64(status.Volume) / 100.0
	c.mpris.UpdateState(statusStr, meta, vol)
}
