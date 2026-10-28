//go:build cgo

// Package opus decodes Opus packets with libopus (cgo, pkg-config opus).
// Built without cgo, NewDecoder returns ErrNoCgo.
package opus

/*
#cgo pkg-config: opus
#include <opus.h>
*/
import "C"
import (
	"errors"
	"fmt"
	"unsafe"
)

// Decoder decodes to interleaved 16-bit PCM.
type Decoder struct {
	dec      *C.OpusDecoder
	channels int
}

func NewDecoder(sampleRate, channels int) (*Decoder, error) {
	var errCode C.int
	dec := C.opus_decoder_create(C.opus_int32(sampleRate), C.int(channels), &errCode)
	if errCode != C.OPUS_OK || dec == nil {
		return nil, fmt.Errorf("opus_decoder_create: %d", int(errCode))
	}
	return &Decoder{dec: dec, channels: channels}, nil
}

// Decode writes one packet's samples into pcm (per channel: at most
// len(pcm)/channels) and returns the samples per channel.
func (d *Decoder) Decode(packet []byte, pcm []int16) (int, error) {
	if d.dec == nil {
		return 0, errors.New("opus: decoder closed")
	}
	if len(packet) == 0 || len(pcm) == 0 {
		return 0, errors.New("opus: empty packet or buffer")
	}
	n := C.opus_decode(d.dec, (*C.uchar)(unsafe.Pointer(&packet[0])), C.opus_int32(len(packet)),
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)/d.channels), 0)
	if n < 0 {
		return 0, fmt.Errorf("opus_decode: %d", int(n))
	}
	return int(n), nil
}

func (d *Decoder) Close() {
	if d.dec != nil {
		C.opus_decoder_destroy(d.dec)
		d.dec = nil
	}
}
