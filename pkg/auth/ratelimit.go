package auth

import (
	"net"
	"strings"
	"sync"
	"time"
)

func cleanIP(ip string) string {
	if host, _, err := net.SplitHostPort(ip); err == nil {
		return host
	}
	return strings.TrimSpace(ip)
}

const (
	LoginWindow      = 15 * time.Minute
	LoginMaxFailures = 5
	LoginLockout     = 15 * time.Minute

	SetupWindow      = 10 * time.Minute
	SetupMaxFailures = 10
	SetupLockout     = 10 * time.Minute

	PairingWindow            = 10 * time.Minute
	PairingIPMaxFailures     = 10
	PairingGlobalMaxFailures = 100

	AuthMinDuration = 300 * time.Millisecond
)

// RateLimiter manages in-memory rate limiting and lockouts for authentication.
type RateLimiter struct {
	mu sync.Mutex

	loginFailures   map[string][]time.Time // key: "u:<username>" or "ip:<ip>"
	setupFailures   map[string][]time.Time // key: ip
	pairingFailures map[string][]time.Time // key: ip
	globalPairing   []time.Time
}

// NewRateLimiter initializes an in-memory RateLimiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		loginFailures:   make(map[string][]time.Time),
		setupFailures:   make(map[string][]time.Time),
		pairingFailures: make(map[string][]time.Time),
	}
}

func pruneTimestamps(history []time.Time, window time.Duration, now time.Time) []time.Time {
	cutoff := now.Add(-window)
	idx := 0
	for idx < len(history) && history[idx].Before(cutoff) {
		idx++
	}
	return history[idx:]
}

// LoginAllowed checks if neither username nor IP is locked out.
func (rl *RateLimiter) LoginAllowed(username, ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	uKey := "u:" + username
	ipKey := "ip:" + cleanIP(ip)

	rl.loginFailures[uKey] = pruneTimestamps(rl.loginFailures[uKey], LoginWindow, now)
	if len(rl.loginFailures[uKey]) >= LoginMaxFailures {
		return false
	}

	rl.loginFailures[ipKey] = pruneTimestamps(rl.loginFailures[ipKey], LoginWindow, now)
	if len(rl.loginFailures[ipKey]) >= LoginMaxFailures {
		return false
	}

	return true
}

// RecordLoginFailure registers a failed login attempt for a username and IP.
func (rl *RateLimiter) RecordLoginFailure(username, ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	uKey := "u:" + username
	ipKey := "ip:" + cleanIP(ip)

	rl.loginFailures[uKey] = append(pruneTimestamps(rl.loginFailures[uKey], LoginWindow, now), now)
	rl.loginFailures[ipKey] = append(pruneTimestamps(rl.loginFailures[ipKey], LoginWindow, now), now)
}

// ResetLoginFailures clears failed login history for a username and IP upon successful authentication.
func (rl *RateLimiter) ResetLoginFailures(username, ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	delete(rl.loginFailures, "u:"+username)
	delete(rl.loginFailures, "ip:"+cleanIP(ip))
}

// SetupCodeAllowed checks if an IP is allowed to attempt the setup code.
func (rl *RateLimiter) SetupCodeAllowed(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	ip = cleanIP(ip)
	rl.setupFailures[ip] = pruneTimestamps(rl.setupFailures[ip], SetupWindow, now)
	return len(rl.setupFailures[ip]) < SetupMaxFailures
}

// RecordSetupFailure records an incorrect setup code attempt from an IP.
func (rl *RateLimiter) RecordSetupFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	ip = cleanIP(ip)
	rl.setupFailures[ip] = append(pruneTimestamps(rl.setupFailures[ip], SetupWindow, now), now)
}

// ResetSetupFailures clears setup failures for an IP.
func (rl *RateLimiter) ResetSetupFailures(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	delete(rl.setupFailures, cleanIP(ip))
}

// PairingCodeAllowed checks if an IP and the global threshold permit pairing code attempts.
func (rl *RateLimiter) PairingCodeAllowed(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	ip = cleanIP(ip)
	rl.pairingFailures[ip] = pruneTimestamps(rl.pairingFailures[ip], PairingWindow, now)
	if len(rl.pairingFailures[ip]) >= PairingIPMaxFailures {
		return false
	}

	rl.globalPairing = pruneTimestamps(rl.globalPairing, PairingWindow, now)
	return len(rl.globalPairing) < PairingGlobalMaxFailures
}

// RecordPairingFailure records an incorrect pairing code attempt.
func (rl *RateLimiter) RecordPairingFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	ip = cleanIP(ip)
	rl.pairingFailures[ip] = append(pruneTimestamps(rl.pairingFailures[ip], PairingWindow, now), now)
	rl.globalPairing = append(pruneTimestamps(rl.globalPairing, PairingWindow, now), now)
}

// EnsureMinDuration sleeps if necessary to ensure authentication responses take at least minDur.
func EnsureMinDuration(start time.Time, minDur time.Duration) {
	if elapsed := time.Since(start); elapsed < minDur {
		time.Sleep(minDur - elapsed)
	}
}
