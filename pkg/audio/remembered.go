package audio

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Volume is a volume control 0-100 (SinkVolume).
type Volume interface {
	Get(ctx context.Context) (int, error)
	Set(ctx context.Context, vol int) error
}

// RememberedVolume keeps each speaker's last level in a file and puts it
// back when the speaker comes up at 0.
//
// Some Bluetooth speakers report an absolute volume of 0 when they connect
// (AVRCP); PipeWire takes it as the speaker's level and WirePlumber saves
// it over the level it remembered. So: for Settle after the speaker
// connects, a 0 is put back to the saved level, and nothing read in that
// time is saved. A level set through the hub is always saved, 0 too: a
// speaker turned all the way down before it left stays down.
type RememberedVolume struct {
	Inner  Volume
	Path   string        // the file: {"<speaker>": level}
	Key    string        // this speaker (its MAC)
	Settle time.Duration // default 10 s
	Log    *slog.Logger

	start time.Time
	mu    sync.Mutex
}

const defaultSettle = 10 * time.Second

// Begin starts a session for the speaker that just connected, and keeps
// its level up for the settle time. Stops with ctx.
func (r *RememberedVolume) Begin(ctx context.Context) {
	if r.Settle <= 0 {
		r.Settle = defaultSettle
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	r.mu.Lock()
	r.start = time.Now()
	r.mu.Unlock()
	saved, ok := r.saved()
	if !ok || saved == 0 {
		return
	}
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		deadline := time.After(r.Settle)
		for {
			if v, err := r.Inner.Get(ctx); err == nil && v == 0 {
				if err := r.Inner.Set(ctx, saved); err == nil {
					r.Log.Info("Speaker came up at 0: put its level back", "speaker", r.Key, "volume", saved)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-deadline:
				return
			case <-t.C:
			}
		}
	}()
}

func (r *RememberedVolume) settling() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.start.IsZero() || time.Since(r.start) < r.Settle
}

// Get reads the level; once settled, a changed level (the speaker's
// buttons, another app) is saved.
func (r *RememberedVolume) Get(ctx context.Context) (int, error) {
	v, err := r.Inner.Get(ctx)
	if err == nil && !r.settling() {
		r.save(v)
	}
	return v, err
}

// Set sets the level and saves it.
func (r *RememberedVolume) Set(ctx context.Context, vol int) error {
	if err := r.Inner.Set(ctx, vol); err != nil {
		return err
	}
	r.save(max(0, min(100, vol)))
	return nil
}

func (r *RememberedVolume) load() map[string]int {
	levels := map[string]int{}
	if data, err := os.ReadFile(r.Path); err == nil {
		json.Unmarshal(data, &levels)
	}
	return levels
}

func (r *RememberedVolume) saved() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.load()[r.Key]
	return v, ok
}

func (r *RememberedVolume) save(v int) {
	if r.Key == "" || r.Path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	levels := r.load()
	if old, ok := levels[r.Key]; ok && old == v {
		return
	}
	levels[r.Key] = v
	data, _ := json.Marshal(levels)
	os.MkdirAll(filepath.Dir(r.Path), 0o755)
	tmp := r.Path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, r.Path)
	}
}
