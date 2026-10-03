package hub

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Binary frames between the board and the hub: byte 0 is the channel, byte 1
// the flags, then the payload. PCM payloads are raw 16-bit little-endian
// mono samples (Opus, later, would be length-prefixed packets).
const (
	chanMic   = 1 // board -> hub, 16 kHz
	chanVoice = 2 // hub -> board, 24 kHz

	voiceRate       = 24000
	voiceChunkBytes = voiceRate * 2 * 60 / 1000 // 60 ms per frame
)

// voiceItem is one ordered message to the board: a PCM chunk (binary) or a
// control message that must not overtake the audio before it (turn_complete,
// interrupted).
type voiceItem struct {
	pcm  []byte
	text []byte
	gen  uint64
}

// voiceQueue sends a reply to the board at a bit more than real time: at
// most `lead` ahead of what the board has played, so its voice buffer (about
// 5 s) never fills and the link never blocks behind audio. flush() drops what
// is queued (the person interrupted).
type voiceQueue struct {
	conn *websocket.Conn
	lead time.Duration

	mu      sync.Mutex
	cond    *sync.Cond
	items   []voiceItem
	gen     uint64
	playEnd time.Time // when the board will have played everything sent
	closed  bool
}

func newVoiceQueue(conn *websocket.Conn, lead time.Duration) *voiceQueue {
	q := &voiceQueue{conn: conn, lead: lead}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *voiceQueue) pushPCM(pcm []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(pcm) > 0 {
		n := min(len(pcm), voiceChunkBytes)
		n &^= 1
		if n == 0 {
			break
		}
		q.items = append(q.items, voiceItem{pcm: pcm[:n:n], gen: q.gen})
		pcm = pcm[n:]
	}
	q.cond.Signal()
}

func (q *voiceQueue) pushText(msg []byte) {
	q.mu.Lock()
	q.items = append(q.items, voiceItem{text: msg, gen: q.gen})
	q.mu.Unlock()
	q.cond.Signal()
}

// flush drops the queued audio (control messages too: the caller sends what
// follows an interrupt itself).
func (q *voiceQueue) flush() {
	q.mu.Lock()
	q.gen++
	q.items = nil
	q.playEnd = time.Time{}
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *voiceQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}

// run writes the queue to the board until ctx ends or the queue is closed.
func (q *voiceQueue) run(ctx context.Context, onErr func(error)) {
	stop := context.AfterFunc(ctx, q.close)
	defer stop()
	frame := make([]byte, 2+voiceChunkBytes)
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		it := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()

		if it.pcm == nil {
			if err := q.write(ctx, websocket.MessageText, it.text); err != nil {
				onErr(err)
				return
			}
			continue
		}
		dur := time.Duration(len(it.pcm)/2) * time.Second / voiceRate
		// Pace: wait until sending keeps the board at most `lead` ahead. A
		// flush (new generation) cancels the wait and the chunk.
		for {
			q.mu.Lock()
			now := time.Now()
			if q.playEnd.Before(now) {
				q.playEnd = now
			}
			wait := q.playEnd.Sub(now) - q.lead
			stale := it.gen != q.gen || q.closed
			if !stale && wait <= 0 {
				q.playEnd = q.playEnd.Add(dur)
			}
			q.mu.Unlock()
			if stale {
				break
			}
			if wait <= 0 {
				frame = append(frame[:0], chanVoice, 0)
				frame = append(frame, it.pcm...)
				if err := q.write(ctx, websocket.MessageBinary, frame); err != nil {
					onErr(err)
					return
				}
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(wait, 20*time.Millisecond)):
			}
		}
	}
}

func (q *voiceQueue) write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return q.conn.Write(wctx, typ, data)
}
