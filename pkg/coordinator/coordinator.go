package coordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"nexus/config"
	"nexus/pkg/audio"
	"nexus/pkg/protocol"
	"nexus/pkg/resolver"
	"nexus/pkg/transport"
)

// Coordinator orchestrates Transport, Resolver, and Audio Engine.
type Coordinator struct {
	cfg       *config.Config
	transport transport.Transport
	resolver  resolver.Resolver
	player    audio.AudioPlayer
	log       *slog.Logger

	stateMu      sync.RWMutex
	currentTrack *resolver.ResolvedTrack
	currentState protocol.PlaybackState
	// Commands are applied in the order Nexus Core sent them, although a
	// play resolves in the background: each play takes a new generation (a
	// later play or a stop makes an older resolve stale), and a pause that
	// arrives while it resolves is applied once it loads.
	playGen    uint64
	pendingID  string // video being resolved ("" when none)
	pendingPos int64
	wantPaused bool
	hubSeq     uint64 // hub mode: the music_play being played

	ctx    context.Context
	cancel context.CancelFunc
}

// NewCoordinator creates a new Coordinator instance.
func NewCoordinator(
	cfg *config.Config,
	trans transport.Transport,
	res resolver.Resolver,
	player audio.AudioPlayer,
) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Coordinator{
		cfg:          cfg,
		transport:    trans,
		resolver:     res,
		player:       player,
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
	if c.transport != nil {
		_ = c.transport.Close()
	}
}

// PlayTrack resolves and plays a YouTube video directly.
func (c *Coordinator) PlayTrack(ctx context.Context, videoID string, startPosMs int64) error {
	return c.playTrack(ctx, c.beginPlay(videoID, startPosMs), videoID, startPosMs)
}

// beginPlay starts a new play generation; older resolves still running are
// dropped when they finish.
func (c *Coordinator) beginPlay(videoID string, startPosMs int64) uint64 {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.playGen++
	c.pendingID = videoID
	c.pendingPos = startPosMs
	c.wantPaused = false
	c.currentState = protocol.StateResolving
	return c.playGen
}

func (c *Coordinator) isCurrentPlay(gen uint64) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return gen == c.playGen
}

func (c *Coordinator) playTrack(ctx context.Context, gen uint64, videoID string, startPosMs int64) error {
	c.log.Info("PlayTrack requested", "videoID", videoID, "startPosMs", startPosMs)

	resolved, err := c.resolver.Resolve(ctx, videoID)
	if !c.isCurrentPlay(gen) {
		c.log.Info("Dropping a superseded play", "videoID", videoID)
		return nil
	}
	if err != nil {
		c.endPending(gen, protocol.StateError)
		c.sendEvent("error", videoID, fmt.Sprintf("resolution failed: %v", err))
		return fmt.Errorf("failed to resolve track %s: %w", videoID, err)
	}

	c.log.Info("Track resolved successfully", "videoID", resolved.VideoID, "title", resolved.Title)
	return c.playResolved(gen, resolved, startPosMs)
}

// playResolved loads a track whose playback URL is known.
func (c *Coordinator) playResolved(gen uint64, resolved *resolver.ResolvedTrack, startPosMs int64) error {
	videoID := resolved.VideoID
	if err := c.player.LoadURL(resolved.PlaybackURL, startPosMs); err != nil {
		c.endPending(gen, protocol.StateError)
		c.sendEvent("error", videoID, fmt.Sprintf("player load failed: %v", err))
		return fmt.Errorf("failed to load audio URL: %w", err)
	}

	// Update mpv media title so mpv's built-in MPRIS displays track details
	if resolved.Title != "" || resolved.Artist != "" {
		_ = c.player.SetMediaTitle(resolved.Title, resolved.Artist)
	}

	c.stateMu.Lock()
	if gen == c.playGen {
		c.currentTrack = resolved
		c.pendingID = ""
		c.currentState = protocol.StatePlaying
	}
	paused := c.wantPaused
	c.stateMu.Unlock()
	if paused {
		_ = c.player.Pause()
	}
	return nil
}

// endPending clears the resolve of generation gen, if it is still current.
func (c *Coordinator) endPending(gen uint64, st protocol.PlaybackState) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if gen == c.playGen {
		c.pendingID = ""
		c.currentState = st
	}
}

// ── Private Handlers ─────────────────────────────────────────────────────────

func (c *Coordinator) setupCallbacks() {
	// The track played to its end, or failed. Nexus Core owns the queue: it
	// sends the next track (or repeats this one) after track_ended.
	c.player.OnTrackEnded(func(err error) {
		c.stateMu.RLock()
		curID := ""
		if c.currentTrack != nil {
			curID = c.currentTrack.VideoID
		}
		c.stateMu.RUnlock()

		if err != nil {
			c.log.Warn("Audio playback failed", "videoID", curID, "err", err)
			c.setLocalState(protocol.StateError)
			c.sendEvent("error", curID, err.Error())
			return
		}
		c.log.Info("Audio playback ended (EOF reached)", "videoID", curID)
		c.setLocalState(protocol.StateIdle)
		c.sendEvent("track_ended", curID, "")
	})

	// MPV status change callback (e.g. keyboard media key toggled pause directly in mpv)
	c.player.OnStatusChanged(func(status audio.PlayerStatus) {
		c.setLocalState(status.State)
		// Immediately emit updated telemetry to Nexus Core
		c.emitTelemetryReport()
	})
}

func (c *Coordinator) handleTransportConnected() {
	if c.hubMode() {
		c.hubHello()
		return
	}
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
			if c.hubMode() {
				// The hub owns the queue and the position; without it this
				// satellite's music stops (docs/PROTOCOL.md section 5.4).
				c.log.Info("Lost the hub: stopping the music")
				c.stop()
			}
		case data := <-msgCh:
			c.handleInboundMessage(data)
		}
	}
}

func (c *Coordinator) handleInboundMessage(data []byte) {
	if c.hubMode() {
		c.handleHubMessage(data)
		return
	}
	var base protocol.BaseMessage
	if err := json.Unmarshal(data, &base); err != nil {
		c.log.Debug("Unrecognized JSON payload", "raw", string(data))
		return
	}

	switch base.Cmd {
	case "orbit_route", "orbit_play":
		// Both carry the track and where to start (position_ms, 0 if absent).
		var play protocol.OrbitRouteMsg
		if err := json.Unmarshal(data, &play); err == nil && play.VideoID != "" {
			c.log.Info("Received "+base.Cmd+" command", "videoId", play.VideoID, "posMs", play.PositionMs)
			gen := c.beginPlay(play.VideoID, play.PositionMs)
			_ = c.player.Stop() // the old track stops now, not when the new one resolves
			go func() {
				_ = c.playTrack(c.ctx, gen, play.VideoID, play.PositionMs)
			}()
		}

	case "orbit_pause":
		c.log.Info("Received orbit_pause command")
		c.pause()

	case "orbit_resume":
		c.log.Info("Received orbit_resume command")
		c.resume()

	case "orbit_seek":
		var seek protocol.OrbitSeekMsg
		if err := json.Unmarshal(data, &seek); err == nil {
			c.log.Info("Received orbit_seek command", "posMs", seek.PositionMs)
			_ = c.player.SeekTo(seek.PositionMs)
		}

	case "orbit_volume":
		var vol protocol.OrbitVolumeMsg
		if err := json.Unmarshal(data, &vol); err == nil {
			c.log.Info("Received orbit_volume command", "volume", vol.Volume)
			_ = c.player.SetVolume(vol.Volume)
		}

	case "orbit_stop":
		c.log.Info("Received orbit_stop command")
		c.stop()

	default:
		c.log.Debug("Ignored inbound command", "cmd", base.Cmd)
	}
}

// pause and resume also apply to a play still resolving (once it loads).
func (c *Coordinator) pause() {
	c.stateMu.Lock()
	c.wantPaused = true
	c.stateMu.Unlock()
	_ = c.player.Pause()
}

func (c *Coordinator) resume() {
	c.stateMu.Lock()
	c.wantPaused = false
	c.stateMu.Unlock()
	_ = c.player.Resume()
}

func (c *Coordinator) stop() {
	c.stateMu.Lock()
	c.playGen++ // a resolve still running is dropped
	c.pendingID = ""
	c.wantPaused = false
	c.currentState = protocol.StateIdle
	c.stateMu.Unlock()
	_ = c.player.Stop()
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
	// While a play resolves, report that track as resolving at its start
	// position (mpv still holds the old track's position).
	if c.pendingID != "" {
		vid = c.pendingID
		status.State = protocol.StateResolving
		status.PositionMs = c.pendingPos
	}
	c.stateMu.RUnlock()

	if c.hubMode() {
		c.hubReport(vid, status.State, status.PositionMs, status.DurationMs)
		return
	}
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
	if c.hubMode() {
		c.hubEvent(eventType, videoID, msg)
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

func (c *Coordinator) setLocalState(st protocol.PlaybackState) {
	c.stateMu.Lock()
	c.currentState = st
	c.stateMu.Unlock()
}
