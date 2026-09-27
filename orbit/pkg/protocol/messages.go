package protocol

import "encoding/json"

// PlaybackState represents current satellite playback state.
type PlaybackState string

const (
	StateIdle      PlaybackState = "IDLE"
	StateResolving PlaybackState = "RESOLVING"
	StateBuffering PlaybackState = "BUFFERING"
	StatePlaying   PlaybackState = "PLAYING"
	StatePaused    PlaybackState = "PAUSED"
	StateStopped   PlaybackState = "STOPPED"
	StateError     PlaybackState = "ERROR"
)

// BaseMessage contains the discriminator "cmd".
type BaseMessage struct {
	Cmd string `json:"cmd"`
}

// SatelliteInfo holds metadata about this satellite client.
type SatelliteInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"` // e.g. "desktop", "raspberry_pi", "laptop"
	Capabilities []string `json:"capabilities"`
}

// SatelliteRegisterMsg is sent upon connecting to Nexus Core.
type SatelliteRegisterMsg struct {
	Cmd       string        `json:"cmd"` // "satellite_register"
	Satellite SatelliteInfo `json:"satellite"`
}

// OrbitRouteMsg directs active playback to this Satellite.
type OrbitRouteMsg struct {
	Cmd        string `json:"cmd"` // "orbit_route"
	Target     string `json:"target,omitempty"`
	VideoID    string `json:"videoId"`
	Title      string `json:"title,omitempty"`
	Artist     string `json:"artist,omitempty"`
	Duration   int    `json:"duration,omitempty"` // in seconds
	PositionMs int64  `json:"position_ms,omitempty"`
}

// OrbitSeekMsg instructs seeking to a millisecond position.
type OrbitSeekMsg struct {
	Cmd        string `json:"cmd"` // "orbit_seek"
	PositionMs int64  `json:"position_ms"`
}

// OrbitVolumeMsg sets satellite playback volume (0-100).
type OrbitVolumeMsg struct {
	Cmd    string `json:"cmd"` // "orbit_volume"
	Volume int    `json:"volume"`
}

// OrbitReportMsg reports periodic telemetry back to Nexus Core.
type OrbitReportMsg struct {
	Cmd         string        `json:"cmd"` // "orbit_report"
	SatelliteID string        `json:"satellite_id"`
	VideoID     string        `json:"video_id,omitempty"`
	State       PlaybackState `json:"state"`
	PositionMs  int64         `json:"position_ms"`
	DurationMs  int64         `json:"duration_ms"`
	Volume      int           `json:"volume"`
}

// OrbitEventMsg reports one-off lifecycle events (e.g. track_ended, buffering, error).
type OrbitEventMsg struct {
	Cmd         string `json:"cmd"` // "orbit_event"
	SatelliteID string `json:"satellite_id"`
	Event       string `json:"event"` // "track_ended", "buffering", "error"
	VideoID     string `json:"video_id,omitempty"`
	Message     string `json:"message,omitempty"`
}

// NexusActionMsg sends user actions (from media keys) to Nexus Core queue.
type NexusActionMsg struct {
	Cmd    string `json:"cmd"`    // "action"
	Action string `json:"action"` // "play", "pause", "toggle", "next", "prev", "stop"
	Value  any    `json:"value,omitempty"`
}

// RawEnvelope allows checking the command before unmarshaling full payload.
type RawEnvelope struct {
	Cmd     string          `json:"cmd"`
	Action  string          `json:"action,omitempty"`
	Payload json.RawMessage `json:"-"`
}
