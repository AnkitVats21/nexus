package hub

import (
	"encoding/json"
	"sort"
	"sync"
)

// Transcript is the hub's copy of the board's conversation transcript.
//
// The board sends {"type":"transcript","seq":N,"entries":[...]} with the
// entries that changed since the last message (all of them after the link
// comes up). Entries are keyed by "id"; a later copy of an entry replaces the
// earlier one. A browser that subscribes gets the whole copy, the same as
// the board sends to a new /api/ws subscriber.
type Transcript struct {
	mu      sync.Mutex
	seq     uint64
	entries map[uint64]json.RawMessage
}

// MaxTranscriptEntries bounds the copy; the board keeps fewer.
const MaxTranscriptEntries = 100

type transcriptMsg struct {
	Seq     uint64            `json:"seq"`
	Entries []json.RawMessage `json:"entries"`
}

type entryID struct {
	ID uint64 `json:"id"`
}

func NewTranscript() *Transcript {
	return &Transcript{entries: map[uint64]json.RawMessage{}}
}

// Reset forgets everything (a board connected: it sends its whole transcript).
func (t *Transcript) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq = 0
	t.entries = map[uint64]json.RawMessage{}
}

// Merge applies one transcript message from the board.
func (t *Transcript) Merge(raw []byte) error {
	var msg transcriptMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range msg.Entries {
		var id entryID
		if err := json.Unmarshal(e, &id); err != nil {
			continue
		}
		t.entries[id.ID] = append(json.RawMessage(nil), e...)
	}
	if msg.Seq > t.seq {
		t.seq = msg.Seq
	}
	for len(t.entries) > MaxTranscriptEntries {
		oldest := ^uint64(0)
		for id := range t.entries {
			if id < oldest {
				oldest = id
			}
		}
		delete(t.entries, oldest)
	}
	return nil
}

// Full is the whole copy as one transcript message, or nil if it is empty.
func (t *Transcript) Full() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) == 0 {
		return nil
	}
	ids := make([]uint64, 0, len(t.entries))
	for id := range t.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := struct {
		Type    string            `json:"type"`
		Seq     uint64            `json:"seq"`
		Entries []json.RawMessage `json:"entries"`
	}{Type: "transcript", Seq: t.seq}
	for _, id := range ids {
		out.Entries = append(out.Entries, t.entries[id])
	}
	b, _ := json.Marshal(out)
	return b
}
