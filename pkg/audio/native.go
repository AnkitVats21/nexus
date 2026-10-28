package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"nexus/pkg/opus"
	"nexus/pkg/protocol"
	"nexus/pkg/webm"
)

// NativePlayer plays WebM/Opus stream URLs (what the hub and the resolvers
// hand out) without mpv: HTTP with Range → pkg/webm → libopus → 48 kHz
// stereo PCM into an output (aplay on the Pi, e.g. -D bluealsa). For small
// machines where mpv doesn't fit (a diskless Pi Zero).
//
// A seek is a Range request from the index's cluster; a connection that
// drops (or idles out while paused) is reopened at the position reached.
// Pause closes the output, so the speaker stops at once and the Bluetooth
// sink is released; resume opens it again.
type NativePlayer struct {
	client *http.Client
	open   OutputFactory
	log    *slog.Logger

	mu       sync.Mutex
	cond     *sync.Cond
	gen      uint64 // each load, seek and stop; an older run sees it and quits
	cancel   context.CancelFunc
	url      string
	hdr      *webm.Header // of hdrURL, kept for seeks
	hdrURL   string
	st       PlayerStatus
	paused   bool
	out      PCMOutput
	onEnded  func(error)
	onStatus func(PlayerStatus)
}

const (
	nativeRate     = 48000
	nativeChannels = 2
	// A read that gets nothing for this long is dropped and reopened (also
	// what happens to the connection during a long pause).
	readIdleTimeout = 20 * time.Second
	maxReconnects   = 3
)

// PCMOutput takes interleaved 16-bit little-endian stereo at 48 kHz.
type PCMOutput interface {
	io.Writer
	Close() error // plays what was written, then closes
	Abort()       // stops now
}

// OutputFactory opens the output when there is audio to play.
type OutputFactory func() (PCMOutput, error)

// NewNativePlayer plays into outputs from open.
func NewNativePlayer(open OutputFactory) *NativePlayer {
	p := &NativePlayer{
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		}},
		open: open,
		log:  slog.Default().With("component", "native_player"),
		st:   PlayerStatus{State: protocol.StateIdle, Volume: 100},
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *NativePlayer) OnTrackEnded(fn func(err error)) {
	p.mu.Lock()
	p.onEnded = fn
	p.mu.Unlock()
}

func (p *NativePlayer) OnStatusChanged(fn func(status PlayerStatus)) {
	p.mu.Lock()
	p.onStatus = fn
	p.mu.Unlock()
}

func (p *NativePlayer) LoadURL(url string, startPosMs int64) error {
	if url == "" {
		return errors.New("no URL")
	}
	p.mu.Lock()
	p.url = url
	p.paused = false
	p.st.DurationMs = 0
	p.startLocked(startPosMs)
	p.mu.Unlock()
	return nil
}

// startLocked starts a run of p.url from startMs, replacing the current one.
func (p *NativePlayer) startLocked(startMs int64) {
	p.stopRunLocked()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.st.State = protocol.StateBuffering
	if p.paused {
		p.st.State = protocol.StatePaused
	}
	p.st.PositionMs = startMs
	go p.run(ctx, p.gen, p.url, startMs)
}

// stopRunLocked ends the current run, if any.
func (p *NativePlayer) stopRunLocked() {
	p.gen++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.out != nil {
		p.out.Abort()
		p.out = nil
	}
	p.cond.Broadcast()
}

func (p *NativePlayer) Pause() error {
	p.mu.Lock()
	p.paused = true
	if p.cancel != nil {
		p.st.State = protocol.StatePaused
		if p.out != nil {
			p.out.Abort()
			p.out = nil
		}
	}
	p.mu.Unlock()
	p.notify()
	return nil
}

func (p *NativePlayer) Resume() error {
	p.mu.Lock()
	p.paused = false
	if p.cancel != nil {
		p.st.State = protocol.StatePlaying
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	p.notify()
	return nil
}

func (p *NativePlayer) SeekTo(posMs int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil || p.url == "" {
		return errors.New("nothing playing")
	}
	if posMs < 0 {
		posMs = 0
	}
	p.startLocked(posMs)
	return nil
}

func (p *NativePlayer) SetVolume(vol int) error {
	p.mu.Lock()
	p.st.Volume = max(0, min(100, vol))
	p.mu.Unlock()
	return nil
}

func (p *NativePlayer) SetMediaTitle(title, artist string) error { return nil }

func (p *NativePlayer) Stop() error {
	p.mu.Lock()
	p.stopRunLocked()
	p.paused = false
	p.st.State = protocol.StateStopped
	p.st.PositionMs = 0
	p.mu.Unlock()
	p.notify()
	return nil
}

func (p *NativePlayer) GetStatus() (PlayerStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st, nil
}

func (p *NativePlayer) Close() error { return p.Stop() }

func (p *NativePlayer) notify() {
	p.mu.Lock()
	fn, st := p.onStatus, p.st
	p.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

// ── The run: one track from a position ──────────────────────────────────────

var errSuperseded = errors.New("superseded")

// retryable: the connection, not the file or the output.
type netError struct{ err error }

func (e netError) Error() string { return e.err.Error() }
func (e netError) Unwrap() error { return e.err }

func (p *NativePlayer) run(ctx context.Context, gen uint64, url string, startMs int64) {
	pos := startMs
	var err error
	for attempt := 0; ; attempt++ {
		err = p.stream(ctx, gen, url, startMs, &pos)
		var ne netError
		if err == nil || ctx.Err() != nil || errors.Is(err, errSuperseded) || !errors.As(err, &ne) ||
			attempt >= maxReconnects {
			break
		}
		p.log.Warn("Stream dropped; reopening", "posMs", pos, "err", err)
		startMs = pos
	}
	p.mu.Lock()
	if gen != p.gen {
		p.mu.Unlock()
		return
	}
	p.cancel = nil
	p.st.State = protocol.StateIdle
	if err != nil {
		p.st.State = protocol.StateError
	}
	fn := p.onEnded
	p.mu.Unlock()
	if err != nil {
		p.log.Warn("Playback failed", "err", err)
	}
	if fn != nil {
		fn(err)
	}
}

// stream plays url from startMs; *pos follows the blocks played.
func (p *NativePlayer) stream(ctx context.Context, gen uint64, url string, startMs int64, pos *int64) error {
	p.mu.Lock()
	hdr := p.hdr
	if p.hdrURL != url {
		hdr = nil
	}
	p.mu.Unlock()

	var rd *webm.Reader
	var body io.ReadCloser
	var total int64
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	openAt := func(at int64) error {
		if body != nil {
			body.Close()
		}
		var err error
		body, total, err = p.get(ctx, url, at)
		return err
	}

	if hdr == nil || startMs == 0 {
		if err := openAt(0); err != nil {
			return err
		}
		rd = webm.NewReader(body)
		h, err := rd.ReadHeader()
		if err != nil {
			return wrapRead(err)
		}
		if !strings.Contains(h.CodecID, "OPUS") {
			return fmt.Errorf("not Opus audio (%s)", h.CodecID)
		}
		hdr = h
		p.mu.Lock()
		if gen == p.gen {
			p.hdr, p.hdrURL = h, url
			p.st.DurationMs = h.DurationMs
		}
		p.mu.Unlock()
	}
	if startMs > 0 {
		cue, ok := hdr.CueAtOrBefore(startMs)
		switch {
		case ok && (rd == nil || cue.Offset > rd.Pos()):
			if err := openAt(cue.Offset); err != nil {
				return err
			}
			rd = webm.NewReaderAt(body, cue.Offset, hdr)
		case rd == nil: // no index: read from the start and skip
			if err := openAt(0); err != nil {
				return err
			}
			rd = webm.NewReader(body)
			if _, err := rd.ReadHeader(); err != nil {
				return wrapRead(err)
			}
		}
	}

	dec, err := opus.NewDecoder(nativeRate, nativeChannels)
	if err != nil {
		return err
	}
	defer dec.Close()

	preskip := 0
	if startMs == 0 {
		preskip = hdr.PreSkip * nativeChannels
	}
	pcm := make([]int16, 5760*nativeChannels) // 120 ms, the longest Opus packet
	buf := make([]byte, 0, len(pcm)*2)
	var out PCMOutput
	defer func() {
		if out != nil {
			out.Abort()
		}
	}()

	for {
		b, err := rd.NextBlock()
		if err == io.EOF {
			if total > 0 && rd.Pos() < total {
				return netError{fmt.Errorf("stream ended at %d of %d bytes", rd.Pos(), total)}
			}
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return errSuperseded
			}
			return wrapRead(err)
		}
		action := webm.Play
		if startMs > 0 {
			action = webm.SeekActionFor(b.TimeMs, startMs)
		}
		if action == webm.Skip {
			continue
		}
		n, err := dec.Decode(b.Data, pcm)
		if err != nil {
			p.log.Debug("Bad packet skipped", "ms", b.TimeMs, "err", err)
			continue
		}
		if action == webm.Preroll {
			continue
		}
		samples := pcm[:n*nativeChannels]
		if preskip > 0 {
			drop := min(preskip, len(samples))
			samples, preskip = samples[drop:], preskip-drop
		}

		// Paused: close the output and wait.
		p.mu.Lock()
		for p.paused && gen == p.gen {
			if out != nil {
				out.Abort()
				out, p.out = nil, nil
			}
			p.cond.Wait()
		}
		if gen != p.gen {
			p.mu.Unlock()
			return errSuperseded
		}
		started := p.st.State != protocol.StatePlaying
		p.st.State = protocol.StatePlaying
		p.st.PositionMs = b.TimeMs
		*pos = b.TimeMs
		vol := p.st.Volume
		p.mu.Unlock()
		if started {
			p.notify()
		}

		if out == nil {
			o, err := p.open()
			if err != nil {
				return fmt.Errorf("audio output: %w", err)
			}
			p.mu.Lock()
			if gen != p.gen || p.paused {
				p.mu.Unlock()
				o.Abort()
				continue
			}
			out, p.out = o, o
			p.mu.Unlock()
		}
		buf = appendPCM(buf[:0], samples, vol)
		if _, err := out.Write(buf); err != nil {
			p.mu.Lock()
			gone := gen != p.gen
			paused := p.paused
			if p.out == out {
				p.out = nil
			}
			p.mu.Unlock()
			if gone {
				return errSuperseded
			}
			out.Abort()
			out = nil
			if paused {
				continue // Pause closed it
			}
			return fmt.Errorf("audio output: %w", err)
		}
	}

	// The end: let the output play what it holds.
	p.mu.Lock()
	if p.out == out {
		p.out = nil
	}
	p.mu.Unlock()
	o := out
	out = nil
	if o != nil {
		if err := o.Close(); err != nil {
			p.mu.Lock()
			gone := gen != p.gen
			p.mu.Unlock()
			if !gone {
				return fmt.Errorf("audio output: %w", err)
			}
		}
	}
	return nil
}

func wrapRead(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || isNetErr(err) {
		return netError{err}
	}
	return err
}

func isNetErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "context canceled") || strings.Contains(s, "EOF")
}

// appendPCM appends samples as 16-bit little-endian at vol (0-100).
func appendPCM(buf []byte, samples []int16, vol int) []byte {
	for _, s := range samples {
		v := s
		if vol < 100 {
			v = int16(int32(s) * int32(vol) / 100)
		}
		buf = append(buf, byte(v), byte(uint16(v)>>8))
	}
	return buf
}

// get opens url from byte `from` (always with a Range header: googlevideo
// throttles requests without one). total is the file's length if known.
func (p *NativePlayer) get(ctx context.Context, url string, from int64) (io.ReadCloser, int64, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, 0, errSuperseded
		}
		return nil, 0, netError{err}
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusOK && from == 0:
	default:
		resp.Body.Close()
		cancel()
		err := fmt.Errorf("HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 500 {
			return nil, 0, netError{err}
		}
		return nil, 0, err // expired or refused URL: the coordinator reports it
	}
	total := resp.ContentLength
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				total = n
			}
		}
	} else if total >= 0 {
		total += from
	}
	return newIdleReader(resp.Body, cancel), total, nil
}

// idleReader cancels its request when no data arrives for readIdleTimeout.
type idleReader struct {
	r      io.ReadCloser
	cancel context.CancelFunc
	timer  *time.Timer
}

func newIdleReader(r io.ReadCloser, cancel context.CancelFunc) *idleReader {
	return &idleReader{r: r, cancel: cancel, timer: time.AfterFunc(readIdleTimeout, cancel)}
}

func (r *idleReader) Read(b []byte) (int, error) {
	r.timer.Reset(readIdleTimeout)
	n, err := r.r.Read(b)
	r.timer.Reset(readIdleTimeout)
	return n, err
}

func (r *idleReader) Close() error {
	r.timer.Stop()
	r.cancel()
	return r.r.Close()
}

// ── aplay ───────────────────────────────────────────────────────────────────

// AplayOutput plays through ALSA's aplay on device (e.g. "default", or
// "bluealsa" for the Bluetooth speaker bluez-alsa last connected).
func AplayOutput(device string) OutputFactory {
	return func() (PCMOutput, error) {
		cmd := exec.Command("aplay", "-q", "-D", device, "-t", "raw", "-f", "S16_LE",
			"-r", strconv.Itoa(nativeRate), "-c", strconv.Itoa(nativeChannels))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &aplayOut{cmd: cmd, stdin: stdin, stderr: &stderr}, nil
	}
}

type aplayOut struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
	once   sync.Once
	err    error
}

func (a *aplayOut) Write(b []byte) (int, error) {
	n, err := a.stdin.Write(b)
	if err != nil {
		a.finish(false)
		if msg := strings.TrimSpace(a.stderr.String()); msg != "" {
			err = fmt.Errorf("%w (aplay: %s)", err, msg)
		}
	}
	return n, err
}

func (a *aplayOut) Close() error { a.finish(true); return a.err }
func (a *aplayOut) Abort()       { a.finish(false) }

func (a *aplayOut) finish(drain bool) {
	a.once.Do(func() {
		if !drain {
			_ = a.cmd.Process.Kill()
		}
		a.stdin.Close()
		err := a.cmd.Wait()
		if drain && err != nil {
			a.err = fmt.Errorf("aplay: %v %s", err, strings.TrimSpace(a.stderr.String()))
		}
	})
}
