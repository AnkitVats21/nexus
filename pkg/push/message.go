package push

import (
	"errors"
)

// MessageType indicates the nature of the push message.
type MessageType string

const (
	TypeCall         MessageType = "call"
	TypeCallEnd      MessageType = "call_end"
	TypeMissed       MessageType = "missed"
	TypeVoiceMessage MessageType = "voice_message"
)

// Message represents a data-only push notification.
// In accordance with FIREBASE.md §3:
// - Data-only messages: no notification block.
// - collapse_key = call_id so a late call never arrives after its call_end.
// - No names, numbers or text in any payload.
// - v=1 is the payload version.
type Message struct {
	Type        MessageType `json:"t"`
	CallID      string      `json:"call_id,omitempty"`
	Reason      string      `json:"reason,omitempty"`       // for call_end: "answered_elsewhere", "cancelled", "timeout"
	MessageID   string      `json:"message_id,omitempty"`   // for voice_message
	Priority    string      `json:"priority,omitempty"`     // optional override: "HIGH" or "NORMAL"
	TTL         string      `json:"ttl,omitempty"`          // optional override: duration ending in 's', e.g. "30s"
	CollapseKey string      `json:"collapse_key,omitempty"` // optional override
}

// AndroidConfig holds the android-specific fields in the FCM HTTP v1 payload.
type AndroidConfig struct {
	Priority    string `json:"priority,omitempty"`
	TTL         string `json:"ttl,omitempty"`
	CollapseKey string `json:"collapse_key,omitempty"`
}

// BuildPayload constructs the data map and AndroidConfig for FCM HTTP v1.
func (m Message) BuildPayload() (map[string]string, AndroidConfig, error) {
	data := map[string]string{
		"t": string(m.Type),
		"v": "1",
	}

	var priority string
	var ttl string
	var collapseKey string

	switch m.Type {
	case TypeCall:
		if m.CallID == "" {
			return nil, AndroidConfig{}, errors.New("call_id required for call message")
		}
		data["call_id"] = m.CallID
		priority = "HIGH"
		ttl = "30s"
		collapseKey = m.CallID

	case TypeCallEnd:
		if m.CallID == "" {
			return nil, AndroidConfig{}, errors.New("call_id required for call_end message")
		}
		data["call_id"] = m.CallID
		if m.Reason != "" {
			data["reason"] = m.Reason
		}
		priority = "NORMAL"
		ttl = "60s"
		collapseKey = m.CallID

	case TypeMissed:
		if m.CallID == "" {
			return nil, AndroidConfig{}, errors.New("call_id required for missed message")
		}
		data["call_id"] = m.CallID
		priority = "NORMAL"
		ttl = "86400s" // 1 day
		collapseKey = m.CallID

	case TypeVoiceMessage:
		if m.MessageID == "" {
			return nil, AndroidConfig{}, errors.New("message_id required for voice_message")
		}
		data["message_id"] = m.MessageID
		priority = "NORMAL"
		ttl = "86400s" // 1 day

	default:
		// Unknown or custom message type
		if m.CallID != "" {
			data["call_id"] = m.CallID
			collapseKey = m.CallID
		}
		if m.MessageID != "" {
			data["message_id"] = m.MessageID
		}
		if m.Reason != "" {
			data["reason"] = m.Reason
		}
		priority = "NORMAL"
		ttl = "60s"
	}

	if m.Priority != "" {
		priority = m.Priority
	}
	if m.TTL != "" {
		ttl = m.TTL
	}
	if m.CollapseKey != "" {
		collapseKey = m.CollapseKey
	}

	android := AndroidConfig{
		Priority:    priority,
		TTL:         ttl,
		CollapseKey: collapseKey,
	}

	return data, android, nil
}
