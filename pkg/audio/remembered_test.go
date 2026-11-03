package audio

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeVolume struct {
	mu  sync.Mutex
	v   int
	set []int
}

func (f *fakeVolume) Get(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v, nil
}

func (f *fakeVolume) Set(_ context.Context, v int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v = v
	f.set = append(f.set, v)
	return nil
}

func (f *fakeVolume) get() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRememberedVolumeRestoresAfterZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "levels.json")
	ctx := context.Background()

	// First session: the hub sets 48; saved.
	f := &fakeVolume{v: 20}
	r := &RememberedVolume{Inner: f, Path: path, Key: "AA", Settle: 50 * time.Millisecond}
	r.Begin(ctx)
	if err := r.Set(ctx, 48); err != nil {
		t.Fatal(err)
	}

	// The speaker reconnects reporting 0: put back to 48.
	f2 := &fakeVolume{v: 0}
	r2 := &RememberedVolume{Inner: f2, Path: path, Key: "AA", Settle: time.Second}
	r2.Begin(ctx)
	waitFor(t, func() bool { return f2.get() == 48 })
	// The 0 read while settling was not saved.
	if v, _ := r2.saved(); v != 48 {
		t.Fatalf("saved %d", v)
	}
}

func TestRememberedVolumeKeepsDeliberateZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "levels.json")
	ctx := context.Background()
	f := &fakeVolume{v: 30}
	r := &RememberedVolume{Inner: f, Path: path, Key: "AA", Settle: 10 * time.Millisecond}
	r.Begin(ctx)
	r.Set(ctx, 0) // turned all the way down

	f2 := &fakeVolume{v: 0}
	r2 := &RememberedVolume{Inner: f2, Path: path, Key: "AA", Settle: 100 * time.Millisecond}
	r2.Begin(ctx)
	time.Sleep(200 * time.Millisecond)
	if len(f2.set) != 0 {
		t.Fatalf("set %v on a speaker left at 0", f2.set)
	}
}

func TestRememberedVolumeSavesButtonChangesAfterSettle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "levels.json")
	ctx := context.Background()
	f := &fakeVolume{v: 0}
	r := &RememberedVolume{Inner: f, Path: path, Key: "BB", Settle: 30 * time.Millisecond}
	r.Begin(ctx)
	r.Get(ctx) // settling: not saved
	if _, ok := r.saved(); ok {
		t.Fatal("saved while settling")
	}
	time.Sleep(50 * time.Millisecond)
	f.Set(ctx, 35) // the speaker's buttons
	r.Get(ctx)
	if v, _ := r.saved(); v != 35 {
		t.Fatalf("saved %d", v)
	}
	// Another speaker's level is its own.
	other := &RememberedVolume{Inner: &fakeVolume{}, Path: path, Key: "CC"}
	if _, ok := other.saved(); ok {
		t.Fatal("CC has a level")
	}
}
