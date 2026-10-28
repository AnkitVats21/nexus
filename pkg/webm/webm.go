// Package webm reads Opus audio from a WebM stream (YouTube's itag 249-251),
// in one pass, from the start or from a Cluster found in the index.
//
// It is the Go side of the board's parser (waveshare
// components/media_player/src/WebmSeek.cpp): YouTube puts the Cues (one per
// Cluster, about every 10 s) before the first Cluster, so the header indexes
// the whole track and a seek is one Range request to the cluster at or before
// the target, then blocks are skipped up to it (SeekAction).
package webm

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

const (
	idEBML          = 0x1A45DFA3
	idSegment       = 0x18538067
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idDuration      = 0x4489
	idTracks        = 0x1654AE6B
	idTrackEntry    = 0xAE
	idTrackNumber   = 0xD7
	idCodecID       = 0x86
	idCodecPrivate  = 0x63A2
	idAudio         = 0xE1
	idChannels      = 0x9F
	idCues          = 0x1C53BB6B
	idCuePoint      = 0xBB
	idCueTime       = 0xB3
	idCueTrackPos   = 0xB7
	idCueClusterPos = 0xF1
	idCluster       = 0x1F43B675
	idTimecode      = 0xE7
	idSimpleBlock   = 0xA3
	idBlockGroup    = 0xA0
	idBlock         = 0xA1

	unknownSize = math.MaxUint64

	// Elements read whole (header parts, blocks); a bigger one is an error.
	maxElement = 4 << 20
)

// SeekPrerollMs: Opus needs about 80 ms of decoded audio to settle after a
// jump (RFC 7845, section 4.6); those frames are decoded and thrown away.
const SeekPrerollMs = 80

// CuePoint is one entry of the index: where a Cluster starts.
type CuePoint struct {
	TimeMs int64 // cluster start, from the track start
	Offset int64 // byte position of the Cluster element in the file
}

// Header is what comes before the first Cluster.
type Header struct {
	CodecID    string
	Track      uint64
	Channels   int
	PreSkip    int // samples at 48 kHz to drop at the start (OpusHead)
	DurationMs int64
	Cues       []CuePoint // empty if the file has no index up front
	scale      uint64     // ns per timecode unit
}

// CueAtOrBefore is the last cue at or before ms, or the first cue if ms
// precedes it. ok is false without an index.
func (h *Header) CueAtOrBefore(ms int64) (CuePoint, bool) {
	if len(h.Cues) == 0 {
		return CuePoint{}, false
	}
	i := sort.Search(len(h.Cues), func(i int) bool { return h.Cues[i].TimeMs > ms })
	if i == 0 {
		return h.Cues[0], true
	}
	return h.Cues[i-1], true
}

// Block is one Opus packet.
type Block struct {
	TimeMs int64
	Data   []byte
}

// SeekAction says what to do with a block while seeking.
type SeekAction int

const (
	Skip    SeekAction = iota // before the pre-roll: drop it undecoded
	Preroll                   // decode it to settle the decoder, discard the audio
	Play                      // at or after the target
)

// SeekActionFor is what to do with a block starting at blockMs while seeking
// to targetMs.
func SeekActionFor(blockMs, targetMs int64) SeekAction {
	switch {
	case blockMs >= targetMs:
		return Play
	case targetMs-blockMs <= SeekPrerollMs:
		return Preroll
	default:
		return Skip
	}
}

// Reader walks a WebM stream element by element: containers (Segment,
// Cluster, BlockGroup) are entered, the header parts read whole, blocks
// returned, the rest skipped. Unknown sizes are allowed on containers.
type Reader struct {
	r          *bufio.Reader
	pos        int64 // file offset of the next byte
	hdr        *Header
	segData    int64 // file offset of the Segment's payload
	clusterTc  int64 // current cluster's timecode
	pendingErr error
}

// NewReader reads from the start of a file; call ReadHeader first.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10)}
}

// NewReaderAt reads from a Cluster at file offset `at` of a file whose header
// is already known (a seek: the body of a Range request from that offset).
func NewReaderAt(r io.Reader, at int64, hdr *Header) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), pos: at, hdr: hdr}
}

// Pos is the file offset of the next unread byte.
func (d *Reader) Pos() int64 { return d.pos }

// Header is the parsed header (nil before ReadHeader).
func (d *Reader) Header() *Header { return d.hdr }

var errShortElement = errors.New("webm: element overruns its data")

func (d *Reader) readByte() (byte, error) {
	b, err := d.r.ReadByte()
	if err == nil {
		d.pos++
	}
	return b, err
}

func (d *Reader) readFull(buf []byte) error {
	n, err := io.ReadFull(d.r, buf)
	d.pos += int64(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (d *Reader) skip(n uint64) error {
	m, err := d.r.Discard(int(n))
	d.pos += int64(m)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// readHeader reads an element ID (marker bit kept) and size. io.EOF only at
// an element boundary.
func (d *Reader) readElementHeader() (id uint32, size uint64, err error) {
	first, err := d.readByte()
	if err != nil {
		return 0, 0, err // io.EOF here is the clean end
	}
	idLen := 1
	for idLen <= 4 && first&(0x80>>(idLen-1)) == 0 {
		idLen++
	}
	if idLen > 4 {
		return 0, 0, fmt.Errorf("webm: bad element id at %d", d.pos-1)
	}
	id = uint32(first)
	for i := 1; i < idLen; i++ {
		b, err := d.readByte()
		if err != nil {
			return 0, 0, io.ErrUnexpectedEOF
		}
		id = id<<8 | uint32(b)
	}
	first, err = d.readByte()
	if err != nil {
		return 0, 0, io.ErrUnexpectedEOF
	}
	szLen := 1
	for szLen <= 8 && first&(0x80>>(szLen-1)) == 0 {
		szLen++
	}
	if szLen > 8 {
		return 0, 0, fmt.Errorf("webm: bad element size at %d", d.pos-1)
	}
	size = uint64(first & (0xFF >> szLen))
	for i := 1; i < szLen; i++ {
		b, err := d.readByte()
		if err != nil {
			return 0, 0, io.ErrUnexpectedEOF
		}
		size = size<<8 | uint64(b)
	}
	if size == (uint64(1)<<(7*szLen))-1 {
		size = unknownSize
	}
	return id, size, nil
}

func (d *Reader) readPayload(id uint32, size uint64) ([]byte, error) {
	if size == unknownSize || size > maxElement {
		return nil, fmt.Errorf("webm: element %#x too large (%d)", id, size)
	}
	buf := make([]byte, size)
	return buf, d.readFull(buf)
}

// ReadHeader reads up to (not into) the first Cluster.
func (d *Reader) ReadHeader() (*Header, error) {
	h := &Header{scale: 1000000, Channels: 2}
	var duration float64
	for {
		id, size, err := d.readElementHeader()
		if err != nil {
			if err == io.EOF {
				return nil, errors.New("webm: no audio")
			}
			return nil, err
		}
		switch id {
		case idSegment:
			d.segData = d.pos // enter
		case idCluster:
			// The first cluster: the header is done. Enter it, so NextBlock
			// carries on from here.
			if h.CodecID == "" {
				return nil, errors.New("webm: no track before the audio")
			}
			cues := h.Cues
			h.Cues = h.Cues[:0]
			for _, c := range cues {
				c.TimeMs = c.TimeMs * int64(h.scale) / 1e6
				c.Offset += d.segData
				if n := len(h.Cues); n > 0 {
					if c.Offset == h.Cues[n-1].Offset {
						continue // another cue in the same cluster
					}
					if c.Offset < h.Cues[n-1].Offset || c.TimeMs < h.Cues[n-1].TimeMs {
						h.Cues = nil // out of order: no index
						break
					}
				}
				h.Cues = append(h.Cues, c)
			}
			if duration > 0 {
				h.DurationMs = int64(duration*float64(h.scale)/1e6 + 0.5)
			}
			d.hdr = h
			return h, nil
		case idInfo, idTracks, idCues:
			data, err := d.readPayload(id, size)
			if err != nil {
				return nil, err
			}
			if err := parseHeaderPart(id, data, h, &duration); err != nil {
				return nil, err
			}
		default:
			if id == idEBML && size == unknownSize {
				return nil, errors.New("webm: bad EBML header")
			}
			if d.pos == 0 && id != idEBML {
				return nil, errors.New("webm: not a WebM file")
			}
			if size == unknownSize {
				return nil, fmt.Errorf("webm: element %#x of unknown size in the header", id)
			}
			if err := d.skip(size); err != nil {
				return nil, err
			}
		}
	}
}

// NextBlock returns the next audio block of the header's track; io.EOF at
// the end of the file.
func (d *Reader) NextBlock() (Block, error) {
	if d.hdr == nil {
		return Block{}, errors.New("webm: header not read")
	}
	for {
		id, size, err := d.readElementHeader()
		if err != nil {
			return Block{}, err
		}
		switch id {
		case idSegment, idCluster, idBlockGroup:
			// enter
		case idTimecode:
			data, err := d.readPayload(id, size)
			if err != nil {
				return Block{}, err
			}
			d.clusterTc = int64(readUint(data))
		case idSimpleBlock, idBlock:
			data, err := d.readPayload(id, size)
			if err != nil {
				return Block{}, err
			}
			track, rel, frame, ok := parseBlock(data)
			if !ok || track != d.hdr.Track {
				continue
			}
			ms := (d.clusterTc + int64(rel)) * int64(d.hdr.scale) / 1e6
			return Block{TimeMs: ms, Data: frame}, nil
		default:
			if size == unknownSize {
				return Block{}, fmt.Errorf("webm: element %#x of unknown size", id)
			}
			if err := d.skip(size); err != nil {
				return Block{}, err
			}
		}
	}
}

// parseBlock splits a (Simple)Block: track number (vint), a signed 16-bit
// timecode relative to the cluster, flags, the frame. Laced blocks (several
// frames in one) are not used for Opus audio and are refused.
func parseBlock(b []byte) (track uint64, rel int16, frame []byte, ok bool) {
	if len(b) == 0 {
		return 0, 0, nil, false
	}
	n := 1
	for n <= 8 && b[0]&(0x80>>(n-1)) == 0 {
		n++
	}
	if n > 8 || len(b) < n+3 {
		return 0, 0, nil, false
	}
	track = uint64(b[0] & (0xFF >> n))
	for i := 1; i < n; i++ {
		track = track<<8 | uint64(b[i])
	}
	rel = int16(binary.BigEndian.Uint16(b[n:]))
	flags := b[n+2]
	if flags&0x06 != 0 {
		return 0, 0, nil, false
	}
	return track, rel, b[n+3:], true
}

func readUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// children walks the elements of a payload read whole.
func children(b []byte, fn func(id uint32, data []byte) error) error {
	d := &Reader{r: bufio.NewReaderSize(byteReader(b), 16)}
	for {
		id, size, err := d.readElementHeader()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if size == unknownSize || size > uint64(len(b))-uint64(d.pos) {
			return errShortElement
		}
		data := b[d.pos : d.pos+int64(size)]
		if err := d.skip(size); err != nil {
			return err
		}
		if err := fn(id, data); err != nil {
			return err
		}
	}
}

type sliceReader struct{ b []byte }

func byteReader(b []byte) io.Reader { return &sliceReader{b} }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// parseHeaderPart reads Info, Tracks or Cues. Cue times and offsets stay
// raw (timecode units, from the Segment's payload) until the first Cluster.
func parseHeaderPart(id uint32, data []byte, h *Header, duration *float64) error {
	switch id {
	case idInfo:
		return children(data, func(id uint32, f []byte) error {
			switch id {
			case idTimecodeScale:
				if s := readUint(f); s > 0 {
					h.scale = s
				}
			case idDuration:
				switch len(f) {
				case 4:
					*duration = float64(math.Float32frombits(binary.BigEndian.Uint32(f)))
				case 8:
					*duration = math.Float64frombits(binary.BigEndian.Uint64(f))
				}
				if !(*duration > 0) {
					*duration = 0
				}
			}
			return nil
		})
	case idTracks:
		return children(data, func(id uint32, entry []byte) error {
			if id != idTrackEntry || h.CodecID != "" {
				return nil // the first track
			}
			return children(entry, func(id uint32, f []byte) error {
				switch id {
				case idTrackNumber:
					h.Track = readUint(f)
				case idCodecID:
					h.CodecID = string(f)
				case idCodecPrivate:
					// OpusHead: magic, version, channels, pre-skip (LE), ...
					if len(f) >= 12 && string(f[:8]) == "OpusHead" {
						h.Channels = int(f[9])
						h.PreSkip = int(binary.LittleEndian.Uint16(f[10:]))
					}
				case idAudio:
					return children(f, func(id uint32, a []byte) error {
						if id == idChannels && h.Channels == 0 {
							h.Channels = int(readUint(a))
						}
						return nil
					})
				}
				return nil
			})
		})
	case idCues:
		return children(data, func(id uint32, point []byte) error {
			if id != idCuePoint {
				return nil
			}
			var c CuePoint
			hasTime, hasPos := false, false
			err := children(point, func(id uint32, f []byte) error {
				switch id {
				case idCueTime:
					c.TimeMs, hasTime = int64(readUint(f)), true
				case idCueTrackPos:
					if hasPos {
						return nil // the first track's position
					}
					return children(f, func(id uint32, t []byte) error {
						if id == idCueClusterPos {
							c.Offset, hasPos = int64(readUint(t)), true
						}
						return nil
					})
				}
				return nil
			})
			if err != nil {
				return err
			}
			if hasTime && hasPos {
				h.Cues = append(h.Cues, c)
			}
			return nil
		})
	}
	return nil
}
