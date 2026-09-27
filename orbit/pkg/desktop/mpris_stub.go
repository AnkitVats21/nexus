//go:build !linux

package desktop

type TrackMetadata struct {
	TrackID      string
	Title        string
	Artist       string
	Album        string
	ArtURL       string
	DurationUsec int64
}

type MPRISManager struct{}

func NewMPRISManager() (*MPRISManager, error) {
	return &MPRISManager{}, nil
}

func (m *MPRISManager) SetHandlers(playPause, play, pause, next, prev, stop func(), seek func(posUsec int64)) {
}

func (m *MPRISManager) UpdateState(playbackStatus string, meta TrackMetadata, vol float64) {
}

func (m *MPRISManager) Close() error {
	return nil
}
