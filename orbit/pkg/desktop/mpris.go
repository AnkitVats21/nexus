//go:build linux

package desktop

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

const (
	mprisPath        = "/org/mpris/MediaPlayer2"
	mprisRootIface   = "org.mpris.MediaPlayer2"
	mprisPlayerIface = "org.mpris.MediaPlayer2.Player"
	mprisServiceName = "org.mpris.MediaPlayer2.nexus_orbit"
)

// TrackMetadata holds metadata exposed over MPRIS.
type TrackMetadata struct {
	TrackID      string
	Title        string
	Artist       string
	Album        string
	ArtURL       string
	DurationUsec int64 // microseconds
}

// MPRISManager manages the Linux MPRIS D-Bus server.
type MPRISManager struct {
	log  *slog.Logger
	conn *dbus.Conn
	prop *prop.Properties

	mu       sync.RWMutex
	playback string
	metadata TrackMetadata
	volume   float64

	onPlayPause func()
	onPlay      func()
	onPause     func()
	onNext      func()
	onPrevious  func()
	onStop      func()
	onSeek      func(posUsec int64)
}

// NewMPRISManager initializes and connects to the user's session D-Bus.
func NewMPRISManager() (*MPRISManager, error) {
	logger := slog.Default().With("component", "mpris")

	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to session bus: %w", err)
	}

	reply, err := conn.RequestName(mprisServiceName, dbus.NameFlagReplaceExisting|dbus.NameFlagDoNotQueue)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to request D-Bus name %s: %w", mprisServiceName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		return nil, fmt.Errorf("name %s already taken on D-Bus", mprisServiceName)
	}

	mgr := &MPRISManager{
		log:      logger,
		conn:     conn,
		playback: "Stopped",
		volume:   1.0,
	}

	if err := mgr.export(); err != nil {
		conn.Close()
		return nil, err
	}

	logger.Info("MPRIS D-Bus service registered", "name", mprisServiceName)
	return mgr, nil
}

func (m *MPRISManager) SetHandlers(playPause, play, pause, next, prev, stop func(), seek func(posUsec int64)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onPlayPause = playPause
	m.onPlay = play
	m.onPause = pause
	m.onNext = next
	m.onPrevious = prev
	m.onStop = stop
	m.onSeek = seek
}

func (m *MPRISManager) export() error {
	rootProps := map[string]*prop.Prop{
		"CanQuit":             {Value: false, Writable: false, Emit: prop.EmitConst},
		"CanRaise":            {Value: false, Writable: false, Emit: prop.EmitConst},
		"HasTrackList":        {Value: false, Writable: false, Emit: prop.EmitConst},
		"Identity":            {Value: "Nexus Orbit Satellite", Writable: false, Emit: prop.EmitConst},
		"SupportedUriSchemes": {Value: []string{"http", "https"}, Writable: false, Emit: prop.EmitConst},
		"SupportedMimeTypes":  {Value: []string{"audio/mpeg", "audio/opus", "audio/webm"}, Writable: false, Emit: prop.EmitConst},
	}

	playerProps := map[string]*prop.Prop{
		"PlaybackStatus": {Value: "Stopped", Writable: false, Emit: prop.EmitTrue},
		"Rate":           {Value: 1.0, Writable: false, Emit: prop.EmitConst},
		"Metadata":       {Value: map[string]dbus.Variant{}, Writable: false, Emit: prop.EmitTrue},
		"Volume":         {Value: 1.0, Writable: true, Emit: prop.EmitTrue},
		"Position":       {Value: int64(0), Writable: false, Emit: prop.EmitFalse},
		"MinimumRate":    {Value: 1.0, Writable: false, Emit: prop.EmitConst},
		"MaximumRate":    {Value: 1.0, Writable: false, Emit: prop.EmitConst},
		"CanControl":     {Value: true, Writable: false, Emit: prop.EmitConst},
		"CanPlay":        {Value: true, Writable: false, Emit: prop.EmitConst},
		"CanPause":       {Value: true, Writable: false, Emit: prop.EmitConst},
		"CanSeek":        {Value: true, Writable: false, Emit: prop.EmitConst},
		"CanGoNext":      {Value: true, Writable: false, Emit: prop.EmitConst},
		"CanGoPrevious":  {Value: true, Writable: false, Emit: prop.EmitConst},
	}

	propsMap := prop.Map{
		mprisRootIface:   rootProps,
		mprisPlayerIface: playerProps,
	}

	props, err := prop.Export(m.conn, dbus.ObjectPath(mprisPath), propsMap)
	if err != nil {
		return fmt.Errorf("failed to export D-Bus properties: %w", err)
	}
	m.prop = props

	// Export method implementations
	if err := m.conn.Export(&playerMethods{mgr: m}, mprisPath, mprisPlayerIface); err != nil {
		return fmt.Errorf("failed to export player methods: %w", err)
	}
	if err := m.conn.Export(&rootMethods{mgr: m}, mprisPath, mprisRootIface); err != nil {
		return fmt.Errorf("failed to export root methods: %w", err)
	}

	return nil
}

// UpdateState updates the MPRIS state and emits change notifications.
func (m *MPRISManager) UpdateState(playbackStatus string, meta TrackMetadata, vol float64) {
	m.mu.Lock()
	m.playback = playbackStatus
	m.metadata = meta
	m.volume = vol
	m.mu.Unlock()

	if m.prop == nil {
		return
	}

	metaMap := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath(fmt.Sprintf("/org/mpris/MediaPlayer2/track/%s", meta.TrackID))),
		"xesam:title":   dbus.MakeVariant(meta.Title),
		"xesam:artist":  dbus.MakeVariant([]string{meta.Artist}),
		"mpris:length":  dbus.MakeVariant(meta.DurationUsec),
	}
	if meta.ArtURL != "" {
		metaMap["mpris:artUrl"] = dbus.MakeVariant(meta.ArtURL)
	}

	_ = m.prop.Set(mprisPlayerIface, "PlaybackStatus", dbus.MakeVariant(playbackStatus))
	_ = m.prop.Set(mprisPlayerIface, "Metadata", dbus.MakeVariant(metaMap))
	_ = m.prop.Set(mprisPlayerIface, "Volume", dbus.MakeVariant(vol))
}

func (m *MPRISManager) Close() error {
	if m.conn != nil {
		_, _ = m.conn.ReleaseName(mprisServiceName)
		return m.conn.Close()
	}
	return nil
}

// ── D-Bus Method Handlers ───────────────────────────────────────────────────

type playerMethods struct {
	mgr *MPRISManager
}

func (p *playerMethods) Next() *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onNext
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn()
	}
	return nil
}

func (p *playerMethods) Previous() *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onPrevious
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn()
	}
	return nil
}

func (p *playerMethods) Pause() *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onPause
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn()
	}
	return nil
}

func (p *playerMethods) PlayPause() *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onPlayPause
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn()
	}
	return nil
}

func (p *playerMethods) Stop() *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onStop
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn()
	}
	return nil
}

func (p *playerMethods) Play() *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onPlay
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn()
	}
	return nil
}

func (p *playerMethods) Seek(offset int64) *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onSeek
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn(offset)
	}
	return nil
}

func (p *playerMethods) SetPosition(trackId dbus.ObjectPath, position int64) *dbus.Error {
	p.mgr.mu.RLock()
	fn := p.mgr.onSeek
	p.mgr.mu.RUnlock()
	if fn != nil {
		go fn(position)
	}
	return nil
}

func (p *playerMethods) OpenUri(uri string) *dbus.Error {
	return dbus.MakeFailedError(errors.New("OpenUri not supported"))
}

type rootMethods struct {
	mgr *MPRISManager
}

func (r *rootMethods) Raise() *dbus.Error {
	return nil
}

func (r *rootMethods) Quit() *dbus.Error {
	return nil
}
