package push

import (
	"context"
	"sync"
)

// SentRecord records a message delivered via FakeSender.
type SentRecord struct {
	DeviceID string
	Message  Message
}

// FakeSender implements Sender for testing, recording all send calls.
type FakeSender struct {
	mu       sync.Mutex
	sent     []SentRecord
	sendFunc func(ctx context.Context, deviceID string, msg Message) error
}

// NewFakeSender initializes a new FakeSender.
func NewFakeSender() *FakeSender {
	return &FakeSender{}
}

// Send records the send attempt and executes custom sendFunc if configured.
func (f *FakeSender) Send(ctx context.Context, deviceID string, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, SentRecord{
		DeviceID: deviceID,
		Message:  msg,
	})
	if f.sendFunc != nil {
		return f.sendFunc(ctx, deviceID, msg)
	}
	return nil
}

// Sent returns a copy of all recorded sent messages.
func (f *FakeSender) Sent() []SentRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SentRecord, len(f.sent))
	copy(out, f.sent)
	return out
}

// SetSendFunc configures a custom callback or error generator for Send.
func (f *FakeSender) SetSendFunc(fn func(ctx context.Context, deviceID string, msg Message) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendFunc = fn
}

// Reset clears recorded sent messages.
func (f *FakeSender) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = nil
}
