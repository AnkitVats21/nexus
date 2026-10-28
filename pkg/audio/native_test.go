//go:build cgo

package audio

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"nexus/pkg/protocol"
)

// fakeOut collects what the player writes, in real time order of opening.
type fakeOut struct {
	mu      sync.Mutex
	opened  int
	aborted int
	written int
	closed  bool
}

func (f *fakeOut) factory() OutputFactory {
	return func() (PCMOutput, error) {
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		return &fakeConn{f: f}, nil
	}
}

func (f *fakeOut) bytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

type fakeConn struct {
	f    *fakeOut
	dead bool
}

func (c *fakeConn) Write(b []byte) (int, error) {
	time.Sleep(time.Millisecond) // a little pacing, so pause can land
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if c.dead {
		return 0, os.ErrClosed
	}
	c.f.written += len(b)
	return len(b), nil
}

func (c *fakeConn) Close() error {
	c.f.mu.Lock()
	c.f.closed = true
	c.f.mu.Unlock()
	return nil
}

func (c *fakeConn) Abort() {
	c.f.mu.Lock()
	c.dead = true
	c.f.aborted++
	c.f.mu.Unlock()
}

// serveTone serves the 25 s test tone with Range support and counts requests.
func serveTone(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	file, err := os.ReadFile("../webm/testdata/tone.webm")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		http.ServeContent(w, r, "tone.webm", time.Time{}, bytes.NewReader(file))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ranges...)
	}
}

func waitEnded(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("track never ended")
		return nil
	}
}

const bytesPerSecond = 48000 * 2 * 2

func TestNativePlaysWholeTrack(t *testing.T) {
	srv, ranges := serveTone(t)
	out := &fakeOut{}
	p := NewNativePlayer(out.factory())
	ended := make(chan error, 1)
	p.OnTrackEnded(func(err error) { ended <- err })

	if err := p.LoadURL(srv.URL, 0); err != nil {
		t.Fatal(err)
	}
	if err := waitEnded(t, ended); err != nil {
		t.Fatal(err)
	}
	got := float64(out.bytes()) / bytesPerSecond
	if got < 24.8 || got > 25.1 {
		t.Fatalf("played %.2f s", got)
	}
	out.mu.Lock()
	closed := out.closed
	out.mu.Unlock()
	if !closed {
		t.Fatal("output not drained at the end")
	}
	if ranges()[0] != "bytes=0-" {
		t.Fatalf("no Range header: %q", ranges()[0])
	}
	st, _ := p.GetStatus()
	if st.State != protocol.StateIdle || st.DurationMs < 24900 {
		t.Fatalf("status %+v", st)
	}
}

// Starting at 12 s: the header, then one Range request from the cue's
// cluster, and the audio from 12 s on.
func TestNativeStartsAtPosition(t *testing.T) {
	srv, ranges := serveTone(t)
	out := &fakeOut{}
	p := NewNativePlayer(out.factory())
	ended := make(chan error, 1)
	p.OnTrackEnded(func(err error) { ended <- err })

	p.LoadURL(srv.URL, 12000)
	if err := waitEnded(t, ended); err != nil {
		t.Fatal(err)
	}
	got := float64(out.bytes()) / bytesPerSecond
	if got < 12.9 || got > 13.1 {
		t.Fatalf("played %.2f s from 12 s", got)
	}
	if len(ranges()) != 2 || ranges()[1] == "bytes=0-" {
		t.Fatalf("requests %q", ranges())
	}
}

func TestNativePauseResumeSeekStop(t *testing.T) {
	srv, ranges := serveTone(t)
	out := &fakeOut{}
	p := NewNativePlayer(out.factory())
	ended := make(chan error, 1)
	p.OnTrackEnded(func(err error) { ended <- err })

	p.LoadURL(srv.URL, 0)
	time.Sleep(100 * time.Millisecond)
	p.Pause()
	time.Sleep(50 * time.Millisecond)
	at := out.bytes()
	time.Sleep(150 * time.Millisecond)
	if out.bytes() != at {
		t.Fatal("still writing while paused")
	}
	if st, _ := p.GetStatus(); st.State != protocol.StatePaused {
		t.Fatalf("state %s", st.State)
	}
	out.mu.Lock()
	aborted := out.aborted
	out.mu.Unlock()
	if aborted == 0 {
		t.Fatal("pause left the output open")
	}

	// Seek while paused: stays paused, header from the cache.
	n := len(ranges())
	p.SeekTo(20000)
	time.Sleep(100 * time.Millisecond)
	if out.bytes() != at {
		t.Fatal("seek while paused played")
	}
	if len(ranges()) != n+1 {
		t.Fatalf("seek took %d requests", len(ranges())-n)
	}
	p.Resume()
	if err := waitEnded(t, ended); err != nil {
		t.Fatal(err)
	}
	if st, _ := p.GetStatus(); st.PositionMs < 24800 {
		t.Fatalf("ended at %d ms", st.PositionMs)
	}

	// Stop: no end event.
	p.LoadURL(srv.URL, 0)
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	select {
	case err := <-ended:
		t.Fatalf("stop reported an end (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if st, _ := p.GetStatus(); st.State != protocol.StateStopped {
		t.Fatalf("state %s", st.State)
	}
}

func TestNativeBadURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p := NewNativePlayer((&fakeOut{}).factory())
	ended := make(chan error, 1)
	p.OnTrackEnded(func(err error) { ended <- err })
	p.LoadURL(srv.URL, 0)
	if err := waitEnded(t, ended); err == nil {
		t.Fatal("no error for a 404")
	}
}
