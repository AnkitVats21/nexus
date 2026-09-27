package audio

import "nexus-orbit/pkg/protocol"

// PlayerStatus represents the current audio playback state.
type PlayerStatus struct {
	State      protocol.PlaybackState `json:"state"`
	PositionMs int64                  `json:"position_ms"`
	DurationMs int64                  `json:"duration_ms"`
	Volume     int                    `json:"volume"`
}

// AudioPlayer defines the interface for local audio rendering engines.
type AudioPlayer interface {
	LoadURL(url string, startPosMs int64) error
	Pause() error
	Resume() error
	Seek(posMs int64) error
	SetVolume(vol int) error
	Stop() error
	GetStatus() (PlayerStatus, error)
	OnTrackEnded(fn func())
	OnStatusChanged(fn func(status PlayerStatus))
	Close() error
}
