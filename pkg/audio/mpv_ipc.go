package audio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"nexus/pkg/protocol"
)

type mpvRequest struct {
	Command   []any `json:"command"`
	RequestID int   `json:"request_id"`
}

type mpvResponse struct {
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
	RequestID int             `json:"request_id"`
	Event     string          `json:"event"`
	Name      string          `json:"name"`
	Reason    string          `json:"reason"`
	FileError string          `json:"file_error"`
}

// MPVIPCClient manages a JSON-IPC connection to a running mpv instance.
type MPVIPCClient struct {
	socketPath string
	log        *slog.Logger

	connMu sync.Mutex
	conn   net.Conn

	reqMu   sync.Mutex
	reqID   int
	pending map[int]chan mpvResponse

	statusMu      sync.RWMutex
	currentStatus PlayerStatus
	paused        bool // mpv's pause property

	onTrackEnded   func(err error)
	onStatusChange func(status PlayerStatus)

	closed  atomic.Bool
	readyCh chan struct{}
}

// NewMPVIPCClient creates a new MPVIPCClient.
func NewMPVIPCClient(socketPath string) *MPVIPCClient {
	c := &MPVIPCClient{
		socketPath: socketPath,
		log:        slog.Default().With("component", "mpv_ipc"),
		pending:    make(map[int]chan mpvResponse),
		readyCh:    make(chan struct{}),
		currentStatus: PlayerStatus{
			State:  protocol.StateIdle,
			Volume: 100,
		},
	}
	return c
}

// Connect dials the mpv unix socket and initializes event subscriptions.
func (c *MPVIPCClient) Connect() error {
	if _, err := os.Stat(c.socketPath); err != nil {
		return fmt.Errorf("mpv socket %s does not exist: %w", c.socketPath, err)
	}

	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("failed to dial mpv socket %s: %w", c.socketPath, err)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	go c.readLoop(conn)

	// Subscribe to key mpv properties
	_ = c.observeProperty(1, "pause")
	_ = c.observeProperty(2, "time-pos")
	_ = c.observeProperty(3, "duration")
	_ = c.observeProperty(4, "volume")
	_ = c.observeProperty(5, "idle-active")

	c.log.Info("Connected to mpv IPC socket", "socket", c.socketPath)
	return nil
}

// OnTrackEnded is called when a track plays to its end (err nil) or fails
// (err set). A track that is stopped or replaced by the next one does not
// count.
func (c *MPVIPCClient) OnTrackEnded(fn func(err error)) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.onTrackEnded = fn
}

func (c *MPVIPCClient) OnStatusChanged(fn func(status PlayerStatus)) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.onStatusChange = fn
}

func (c *MPVIPCClient) LoadURL(url string, startPosMs int64) error {
	c.log.Info("Loading audio URL into mpv", "url", url, "startPosMs", startPosMs)

	c.statusMu.Lock()
	c.currentStatus.State = protocol.StateBuffering
	c.currentStatus.PositionMs = startPosMs
	c.statusMu.Unlock()
	c.notifyStatusChange()

	var err error
	if startPosMs > 0 {
		startSecs := fmt.Sprintf("%.2f", float64(startPosMs)/1000.0)
		_, err = c.sendCommand("loadfile", url, "replace", -1, "start="+startSecs)
	} else {
		_, err = c.sendCommand("loadfile", url, "replace")
	}

	if err != nil {
		c.statusMu.Lock()
		c.currentStatus.State = protocol.StateError
		c.statusMu.Unlock()
		c.notifyStatusChange()
		return fmt.Errorf("mpv loadfile failed: %w", err)
	}

	// Ensure unpaused
	_ = c.setProperty("pause", false)
	return nil
}

func (c *MPVIPCClient) Pause() error {
	c.log.Debug("Pausing mpv playback")
	err := c.setProperty("pause", true)
	if err == nil {
		c.statusMu.Lock()
		c.currentStatus.State = protocol.StatePaused
		c.statusMu.Unlock()
		c.notifyStatusChange()
	}
	return err
}

func (c *MPVIPCClient) Resume() error {
	c.log.Debug("Resuming mpv playback")
	err := c.setProperty("pause", false)
	if err == nil {
		c.statusMu.Lock()
		c.currentStatus.State = protocol.StatePlaying
		c.statusMu.Unlock()
		c.notifyStatusChange()
	}
	return err
}

func (c *MPVIPCClient) SeekTo(posMs int64) error {
	posSecs := float64(posMs) / 1000.0
	c.log.Debug("Seeking mpv playback", "posMs", posMs, "posSecs", posSecs)
	_, err := c.sendCommand("seek", posSecs, "absolute")
	if err == nil {
		c.statusMu.Lock()
		c.currentStatus.PositionMs = posMs
		c.statusMu.Unlock()
		c.notifyStatusChange()
	}
	return err
}

func (c *MPVIPCClient) SetVolume(vol int) error {
	if vol < 0 {
		vol = 0
	} else if vol > 100 {
		vol = 100
	}
	c.log.Debug("Setting mpv volume", "volume", vol)
	err := c.setProperty("volume", vol)
	if err == nil {
		c.statusMu.Lock()
		c.currentStatus.Volume = vol
		c.statusMu.Unlock()
		c.notifyStatusChange()
	}
	return err
}

func (c *MPVIPCClient) SetMediaTitle(title, artist string) error {
	displayTitle := title
	if artist != "" && title != "" {
		displayTitle = fmt.Sprintf("%s - %s", artist, title)
	} else if title == "" {
		displayTitle = artist
	}
	if displayTitle == "" {
		return nil
	}
	c.log.Debug("Setting mpv force-media-title", "title", displayTitle)
	return c.setProperty("force-media-title", displayTitle)
}

func (c *MPVIPCClient) Stop() error {
	c.log.Debug("Stopping mpv playback")
	_, err := c.sendCommand("stop")
	c.statusMu.Lock()
	c.currentStatus.State = protocol.StateStopped
	c.currentStatus.PositionMs = 0
	c.statusMu.Unlock()
	c.notifyStatusChange()
	return err
}

func (c *MPVIPCClient) GetStatus() (PlayerStatus, error) {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.currentStatus, nil
}

func (c *MPVIPCClient) Close() error {
	c.closed.Store(true)
	c.connMu.Lock()
	defer c.connMu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// ── Low-level command & event handling ──────────────────────────────────────

func (c *MPVIPCClient) sendCommand(args ...any) (json.RawMessage, error) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return nil, errors.New("mpv IPC socket not connected")
	}

	c.reqMu.Lock()
	c.reqID++
	id := c.reqID
	replyCh := make(chan mpvResponse, 1)
	c.pending[id] = replyCh
	c.reqMu.Unlock()

	req := mpvRequest{
		Command:   args,
		RequestID: id,
	}

	data, err := json.Marshal(req)
	if err != nil {
		c.removePending(id)
		return nil, err
	}
	data = append(data, '\n')

	if _, err := conn.Write(data); err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("write to mpv socket failed: %w", err)
	}

	select {
	case resp := <-replyCh:
		if resp.Error != "success" && resp.Error != "" {
			return nil, fmt.Errorf("mpv command error: %s", resp.Error)
		}
		return resp.Data, nil
	case <-time.After(3 * time.Second):
		c.removePending(id)
		return nil, errors.New("timeout waiting for mpv response")
	}
}

func (c *MPVIPCClient) setProperty(name string, val any) error {
	_, err := c.sendCommand("set_property", name, val)
	return err
}

func (c *MPVIPCClient) observeProperty(id int, name string) error {
	_, err := c.sendCommand("observe_property", id, name)
	return err
}

func (c *MPVIPCClient) removePending(id int) {
	c.reqMu.Lock()
	delete(c.pending, id)
	c.reqMu.Unlock()
}

func (c *MPVIPCClient) readLoop(conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp mpvResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			c.log.Warn("Failed to parse mpv IPC JSON", "err", err, "raw", string(line))
			continue
		}

		if resp.RequestID > 0 {
			c.reqMu.Lock()
			if ch, ok := c.pending[resp.RequestID]; ok {
				ch <- resp
				delete(c.pending, resp.RequestID)
			}
			c.reqMu.Unlock()
			continue
		}

		if resp.Event != "" {
			c.handleEvent(resp)
		}
	}

	if !c.closed.Load() {
		c.log.Warn("Disconnected from mpv IPC socket")
	}
}

func (c *MPVIPCClient) handleEvent(resp mpvResponse) {
	switch resp.Event {
	case "property-change":
		c.handlePropertyChange(resp.Name, resp.Data)
	case "playback-restart":
		// Audio is playing (after a load or a seek): buffering is over.
		c.statusMu.Lock()
		changed := c.currentStatus.State == protocol.StateBuffering
		if changed {
			if c.paused {
				c.currentStatus.State = protocol.StatePaused
			} else {
				c.currentStatus.State = protocol.StatePlaying
			}
		}
		c.statusMu.Unlock()
		if changed {
			c.notifyStatusChange()
		}
	case "end-file":
		// "stop" (stopped, or replaced by loadfile) and "quit" are not an end
		// of the track: Nexus Core would advance its queue on one.
		c.log.Info("MPV end-file event", "reason", resp.Reason)
		switch resp.Reason {
		case "eof":
			c.setState(protocol.StateIdle)
			c.notifyTrackEnded(nil)
		case "error":
			c.setState(protocol.StateError)
			c.notifyTrackEnded(fmt.Errorf("mpv: %s", resp.FileError))
		}
	}
}

func (c *MPVIPCClient) handlePropertyChange(name string, raw json.RawMessage) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()

	switch name {
	case "time-pos":
		var pos float64
		if err := json.Unmarshal(raw, &pos); err == nil && !math.IsNaN(pos) {
			c.currentStatus.PositionMs = int64(pos * 1000)
		}
	case "duration":
		var dur float64
		if err := json.Unmarshal(raw, &dur); err == nil && !math.IsNaN(dur) {
			c.currentStatus.DurationMs = int64(dur * 1000)
		}
	case "pause":
		var paused bool
		if err := json.Unmarshal(raw, &paused); err == nil {
			c.paused = paused
			if paused {
				if c.currentStatus.State == protocol.StatePlaying || c.currentStatus.State == protocol.StateBuffering {
					c.currentStatus.State = protocol.StatePaused
				}
			} else {
				if c.currentStatus.State == protocol.StatePaused || c.currentStatus.State == protocol.StateBuffering {
					c.currentStatus.State = protocol.StatePlaying
				}
			}
		}
	case "volume":
		var vol float64
		if err := json.Unmarshal(raw, &vol); err == nil {
			c.currentStatus.Volume = int(vol)
		}
	case "idle-active":
		var isIdle bool
		if err := json.Unmarshal(raw, &isIdle); err == nil {
			if isIdle {
				// The end of a track is reported from end-file, which says why.
				c.currentStatus.State = protocol.StateIdle
			}
		}
	}
}

func (c *MPVIPCClient) setState(st protocol.PlaybackState) {
	c.statusMu.Lock()
	c.currentStatus.State = st
	c.statusMu.Unlock()
	c.notifyStatusChange()
}

func (c *MPVIPCClient) notifyTrackEnded(err error) {
	c.statusMu.RLock()
	fn := c.onTrackEnded
	c.statusMu.RUnlock()

	if fn != nil {
		fn(err)
	}
}

func (c *MPVIPCClient) notifyStatusChange() {
	c.statusMu.RLock()
	fn := c.onStatusChange
	st := c.currentStatus
	c.statusMu.RUnlock()

	if fn != nil {
		fn(st)
	}
}
