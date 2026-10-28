//go:build cgo

package opus

import (
	"bytes"
	"io"
	"os"
	"testing"

	"nexus/pkg/webm"
)

// Decodes the WebM test tone: every packet is 20 ms at 48 kHz, and the
// signal is a loud 440 Hz sine.
func TestDecodeTone(t *testing.T) {
	file, err := os.ReadFile("../webm/testdata/tone.webm")
	if err != nil {
		t.Fatal(err)
	}
	r := webm.NewReader(bytes.NewReader(file))
	if _, err := r.ReadHeader(); err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm := make([]int16, 5760*2)
	var peak int16
	blocks := 0
	for {
		b, err := r.NextBlock()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n, err := dec.Decode(b.Data, pcm)
		if err != nil {
			t.Fatalf("block %d: %v", blocks, err)
		}
		if n != 960 {
			t.Fatalf("block %d: %d samples", blocks, n)
		}
		for _, s := range pcm[:n*2] {
			if s > peak {
				peak = s
			}
		}
		blocks++
	}
	if peak < 2000 {
		t.Fatalf("peak %d: silent?", peak)
	}
}
