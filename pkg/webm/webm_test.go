package webm

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// testdata (ffmpeg, libopus):
//   tone.webm:   25 s stereo, cues up front (-cues_to_front 1), 5 s clusters
//   nocues.webm: 3 s mono, cues at the end (the default)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readAll(t *testing.T, d *Reader) []Block {
	t.Helper()
	var out []Block
	for {
		b, err := d.NextBlock()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("block %d: %v", len(out), err)
		}
		out = append(out, b)
	}
}

func TestHeaderAndBlocks(t *testing.T) {
	d := NewReader(bytes.NewReader(load(t, "tone.webm")))
	h, err := d.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if h.CodecID != "A_OPUS" || h.Channels != 2 || h.PreSkip == 0 {
		t.Fatalf("header %+v", h)
	}
	if h.DurationMs < 24900 || h.DurationMs > 25100 {
		t.Fatalf("duration %d", h.DurationMs)
	}
	if len(h.Cues) < 4 || h.Cues[0].TimeMs != 0 {
		t.Fatalf("cues %+v", h.Cues)
	}
	blocks := readAll(t, d)
	// 20 ms frames.
	if n := len(blocks); n < 1240 || n > 1260 {
		t.Fatalf("%d blocks", n)
	}
	for i := 1; i < len(blocks); i++ {
		if blocks[i].TimeMs < blocks[i-1].TimeMs {
			t.Fatalf("time goes back at %d", i)
		}
	}
	if last := blocks[len(blocks)-1].TimeMs; last < 24800 {
		t.Fatalf("last block at %d ms", last)
	}
}

// A seek reads from the cue's offset with the header already known, as a
// Range request would, and lands on the cluster's first block.
func TestSeekFromCue(t *testing.T) {
	file := load(t, "tone.webm")
	h, err := NewReader(bytes.NewReader(file)).ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{0, 7300, 12000, 24000} {
		cue, ok := h.CueAtOrBefore(target)
		if !ok || cue.TimeMs > target {
			t.Fatalf("cue for %d: %+v", target, cue)
		}
		d := NewReaderAt(bytes.NewReader(file[cue.Offset:]), cue.Offset, h)
		b, err := d.NextBlock()
		if err != nil {
			t.Fatalf("target %d: %v", target, err)
		}
		if b.TimeMs != cue.TimeMs {
			t.Fatalf("target %d: first block at %d, cue at %d", target, b.TimeMs, cue.TimeMs)
		}
	}
}

func TestNoCuesUpFront(t *testing.T) {
	d := NewReader(bytes.NewReader(load(t, "nocues.webm")))
	h, err := d.ReadHeader()
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Cues) != 0 || h.Channels != 1 {
		t.Fatalf("header %+v", h)
	}
	if _, ok := h.CueAtOrBefore(1000); ok {
		t.Fatal("cue without an index")
	}
	if n := len(readAll(t, d)); n < 145 {
		t.Fatalf("%d blocks", n)
	}
}

func TestTruncated(t *testing.T) {
	file := load(t, "tone.webm")
	d := NewReader(bytes.NewReader(file[:len(file)/2]))
	if _, err := d.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	var err error
	for err == nil {
		_, err = d.NextBlock()
	}
	if err == io.EOF {
		t.Fatal("a cut stream ended cleanly")
	}
}

func TestNotWebM(t *testing.T) {
	if _, err := NewReader(bytes.NewReader([]byte("OggS\x00\x02 not webm at all"))).ReadHeader(); err == nil {
		t.Fatal("no error")
	}
}

func TestSeekAction(t *testing.T) {
	cases := []struct {
		block, target int64
		want          SeekAction
	}{
		{1000, 1000, Play}, {1020, 1000, Play}, {920, 1000, Preroll}, {919, 1000, Skip}, {0, 5000, Skip},
	}
	for _, c := range cases {
		if got := SeekActionFor(c.block, c.target); got != c.want {
			t.Errorf("%d→%d: %v, want %v", c.block, c.target, got, c.want)
		}
	}
}
