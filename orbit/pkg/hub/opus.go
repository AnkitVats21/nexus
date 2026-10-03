package hub

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

const (
	opusAppVoIP               = int(C.OPUS_APPLICATION_VOIP)
	opusAppAudio              = int(C.OPUS_APPLICATION_AUDIO)
	opusAppRestrictedLowdelay = int(C.OPUS_APPLICATION_RESTRICTED_LOWDELAY)
)

type opusDecoder struct {
	dec *C.OpusDecoder
}

func newOpusDecoder(sampleRate, channels int) (*opusDecoder, error) {
	var errCode C.int
	dec := C.opus_decoder_create(C.opus_int32(sampleRate), C.int(channels), &errCode)
	if errCode != C.OPUS_OK || dec == nil {
		return nil, fmt.Errorf("opus_decoder_create failed: %d", int(errCode))
	}
	return &opusDecoder{dec: dec}, nil
}

func (d *opusDecoder) Decode(data []byte, pcm []int16) (int, error) {
	if d.dec == nil {
		return 0, errors.New("decoder is closed")
	}
	var dataPtr *C.uchar
	if len(data) > 0 {
		dataPtr = (*C.uchar)(unsafe.Pointer(&data[0]))
	}
	var pcmPtr *C.opus_int16
	if len(pcm) > 0 {
		pcmPtr = (*C.opus_int16)(unsafe.Pointer(&pcm[0]))
	}
	n := C.opus_decode(
		d.dec,
		dataPtr,
		C.opus_int32(len(data)),
		pcmPtr,
		C.int(len(pcm)),
		0,
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_decode error: %d", int(n))
	}
	return int(n), nil
}

func (d *opusDecoder) Close() {
	if d.dec != nil {
		C.opus_decoder_destroy(d.dec)
		d.dec = nil
	}
}

type opusEncoder struct {
	enc *C.OpusEncoder
}

func newOpusEncoder(sampleRate, channels, app int) (*opusEncoder, error) {
	var errCode C.int
	enc := C.opus_encoder_create(C.opus_int32(sampleRate), C.int(channels), C.int(app), &errCode)
	if errCode != C.OPUS_OK || enc == nil {
		return nil, fmt.Errorf("opus_encoder_create failed: %d", int(errCode))
	}
	return &opusEncoder{enc: enc}, nil
}

func (e *opusEncoder) Encode(pcm []int16, frameSize int, out []byte) (int, error) {
	if e.enc == nil {
		return 0, errors.New("encoder is closed")
	}
	if len(pcm) < frameSize {
		return 0, errors.New("pcm slice smaller than frameSize")
	}
	n := C.opus_encode(
		e.enc,
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])),
		C.int(frameSize),
		(*C.uchar)(unsafe.Pointer(&out[0])),
		C.opus_int32(len(out)),
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_encode error: %d", int(n))
	}
	return int(n), nil
}

func (e *opusEncoder) Close() {
	if e.enc != nil {
		C.opus_encoder_destroy(e.enc)
		e.enc = nil
	}
}
