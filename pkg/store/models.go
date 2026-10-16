package store

import "errors"

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict: record already exists")
)

// User represents an authenticated hub user.
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`   // "admin" or "member"
	Status      string `json:"status"` // "pending", "active", "disabled"
	PassHash    string `json:"-"`
	MustChange  bool   `json:"must_change"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Session represents an active web session bound to a user.
type Session struct {
	IDHash    [32]byte `json:"-"`
	UserID    int64    `json:"user_id"`
	CreatedAt int64    `json:"created_at"`
	ExpiresAt int64    `json:"expires_at"`
	UserAgent string   `json:"user_agent,omitempty"`
	IP        string   `json:"ip,omitempty"`
}

// Device represents a registered edge device bound to an owner user.
type Device struct {
	ID        string   `json:"id"`
	OwnerID   int64    `json:"owner_id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	TokenHash [32]byte `json:"-"`
	CreatedAt int64    `json:"created_at"`
	LastSeen  int64    `json:"last_seen"`
	LastFW    string   `json:"last_fw"`
}

// PairingCode represents an active one-time pairing code issued to a user.
type PairingCode struct {
	CodeHash  [32]byte `json:"-"`
	UserID    int64    `json:"user_id"`
	ExpiresAt int64    `json:"expires_at"`
	CreatedAt int64    `json:"created_at"`
}

// PushToken represents an FCM push registration token for a device.
type PushToken struct {
	DeviceID  string `json:"device_id"`
	Provider  string `json:"provider"`
	Token     string `json:"-"`
	UpdatedAt int64  `json:"updated_at"`
	LastOKAt  int64  `json:"last_ok_at,omitempty"`
	FailCount int    `json:"fail_count"`
}

// RefreshToken represents an active or used rotating refresh token record.
type RefreshToken struct {
	IDHash    [32]byte `json:"-"`
	UserID    int64    `json:"user_id"`
	DeviceID  *string  `json:"device_id,omitempty"`
	FamilyID  string   `json:"family_id"`
	CreatedAt int64    `json:"created_at"`
	ExpiresAt int64    `json:"expires_at"`
	UsedAt    *int64   `json:"used_at,omitempty"`
	UserAgent string   `json:"user_agent,omitempty"`
	IP        string   `json:"ip,omitempty"`
}
