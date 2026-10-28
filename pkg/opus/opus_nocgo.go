//go:build !cgo

package opus

type Decoder struct{}

func NewDecoder(sampleRate, channels int) (*Decoder, error) { return nil, ErrNoCgo }

func (d *Decoder) Decode(packet []byte, pcm []int16) (int, error) { return 0, ErrNoCgo }

func (d *Decoder) Close() {}
