package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"nexus/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStoreAndAuth(t *testing.T) (*store.Store, *Service, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nexus-auth-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("Open store: %v", err)
	}
	svc, err := NewService(st, "127.0.0.1", "http://hub.local:8380")
	if err != nil {
		st.Close()
		os.RemoveAll(dir)
		t.Fatalf("NewService: %v", err)
	}
	cleanup := func() {
		st.Close()
		os.RemoveAll(dir)
	}
	return st, svc, cleanup
}

func TestPasswordHashingAndVerification(t *testing.T) {
	// Password too short
	if _, err := HashPassword("short"); err != ErrPasswordTooShort {
		t.Fatalf("expected ErrPasswordTooShort, got %v", err)
	}

	rawPass := "CorrectHorseBatteryStaple123!"
	encoded, err := HashPassword(rawPass)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected hash header: %s", encoded)
	}

	// Verify valid password
	ok, err := VerifyPassword(rawPass, encoded)
	if err != nil || !ok {
		t.Fatalf("expected valid verification, got ok=%v, err=%v", ok, err)
	}

	// Verify wrong password
	ok, err = VerifyPassword("WrongPassword123!", encoded)
	if err != nil || ok {
		t.Fatalf("expected false for wrong password, got ok=%v, err=%v", ok, err)
	}
}

func TestUsernameValidation(t *testing.T) {
	valid := []string{"admin", "riya", "john.doe", "user-name_1", "ab", "12345678901234567890123456789012"}
	for _, u := range valid {
		if err := ValidateUsername(u); err != nil {
			t.Errorf("expected valid username %q, got err=%v", u, err)
		}
	}

	invalid := []string{"a", "has space", "admin@domain", "user#1", "", strings.Repeat("a", 33)}
	for _, u := range invalid {
		if err := ValidateUsername(u); err == nil {
			t.Errorf("expected invalid username %q, got nil error", u)
		}
	}
}

func TestSetupFlowAndRateLimits(t *testing.T) {
	_, svc, cleanup := newTestStoreAndAuth(t)
	defer cleanup()

	ctx := context.Background()

	needed, err := svc.SetupNeeded(ctx)
	if err != nil || !needed {
		t.Fatalf("expected setup needed, got needed=%v, err=%v", needed, err)
	}

	code := svc.SetupCode()
	if code == "" {
		t.Fatal("expected non-empty setup code")
	}

	// Test wrong setup code failure
	_, _, _, err = svc.Setup(ctx, "999-999", "admin", "Admin", "SecretPass123!", "Agent", "127.0.0.1")
	if err != ErrInvalidSetupCode {
		t.Fatalf("expected ErrInvalidSetupCode, got %v", err)
	}

	// Rate limit setup code (10 failures)
	for i := 0; i < 9; i++ {
		_, _, _, _ = svc.Setup(ctx, "999-999", "admin", "Admin", "SecretPass123!", "Agent", "127.0.0.1")
	}
	_, _, _, err = svc.Setup(ctx, code, "admin", "Admin", "SecretPass123!", "Agent", "127.0.0.1")
	if err != ErrTooManyAttempts {
		t.Fatalf("expected ErrTooManyAttempts after 10 failed codes, got %v", err)
	}

	// Different IP should still be allowed
	u, token, _, err := svc.Setup(ctx, code, "admin", "Admin", "SecretPass123!", "Agent", "10.0.0.2")
	if err != nil {
		t.Fatalf("Setup from different IP failed: %v", err)
	}
	if u.Role != "admin" || token == "" {
		t.Fatalf("unexpected setup result: user=%+v, token=%s", u, token)
	}

	// Once setup, code is cleared and setup is no longer needed
	if svc.SetupCode() != "" {
		t.Fatalf("expected setup code cleared, got %s", svc.SetupCode())
	}
	needed, _ = svc.SetupNeeded(ctx)
	if needed {
		t.Fatal("expected setup not needed after admin creation")
	}

	// Re-attempting setup fails
	_, _, _, err = svc.Setup(ctx, code, "admin2", "Admin2", "SecretPass123!", "Agent", "10.0.0.2")
	if err != ErrSetupNotNeeded {
		t.Fatalf("expected ErrSetupNotNeeded, got %v", err)
	}
}

func TestLoginAndSessions(t *testing.T) {
	_, svc, cleanup := newTestStoreAndAuth(t)
	defer cleanup()

	ctx := context.Background()
	code := svc.SetupCode()
	_, _, _, err := svc.Setup(ctx, code, "admin", "Admin", "AdminSecret123!", "Mozilla", "127.0.0.1")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Successful login
	u, token, _, err := svc.Login(ctx, "admin", "AdminSecret123!", "Firefox", "127.0.0.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if u.Username != "admin" {
		t.Fatalf("unexpected user: %+v", u)
	}

	// Authenticate session
	hash := TokenHash(token)
	authU, sess, err := svc.AuthenticateSession(ctx, hash)
	if err != nil {
		t.Fatalf("AuthenticateSession: %v", err)
	}
	if authU.ID != u.ID || sess.UserAgent != "Firefox" {
		t.Fatalf("unexpected authenticated session: user=%+v, sess=%+v", authU, sess)
	}

	// Change password and ensure other sessions are revoked
	_, hash2, _ := GenerateSessionToken()
	now := time.Now().Unix()
	_ = svc.Store().CreateSession(ctx, &store.Session{
		IDHash:    hash2,
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now + 3600,
	})

	if err := svc.ChangePassword(ctx, u.ID, "AdminSecret123!", "NewSecretPass456!", hash); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// Token 1 should remain valid
	if _, _, err := svc.AuthenticateSession(ctx, hash); err != nil {
		t.Fatalf("expected current session to remain valid, got %v", err)
	}

	// Token 2 should be revoked
	if _, _, err := svc.AuthenticateSession(ctx, hash2); err != ErrUnauthorized {
		t.Fatalf("expected other session to be revoked, got %v", err)
	}

	// Logout token 1
	if err := svc.Logout(ctx, hash); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, _, err := svc.AuthenticateSession(ctx, hash); err != ErrUnauthorized {
		t.Fatalf("expected logged out session to be unauthorized, got %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, svc, cleanup := newTestStoreAndAuth(t)
	defer cleanup()

	ctx := context.Background()
	code := svc.SetupCode()
	_, _, _, _ = svc.Setup(ctx, code, "bob", "Bob", "SecretPass123!", "Mozilla", "127.0.0.1")

	// 5 failed login attempts
	for i := 0; i < 5; i++ {
		_, _, _, err := svc.Login(ctx, "bob", "WrongPass123!", "Mozilla", "127.0.0.1")
		if err != ErrInvalidCredentials {
			t.Fatalf("attempt %d: expected ErrInvalidCredentials, got %v", i, err)
		}
	}

	// 6th attempt with correct password should be locked out
	_, _, _, err := svc.Login(ctx, "bob", "SecretPass123!", "Mozilla", "127.0.0.1")
	if err != ErrInvalidCredentials {
		t.Fatalf("expected lockout (generic invalid credentials), got %v", err)
	}

	// Different IP still locked because username is locked
	_, _, _, err = svc.Login(ctx, "bob", "SecretPass123!", "Mozilla", "192.168.1.100")
	if err != ErrInvalidCredentials {
		t.Fatalf("expected username lockout across IPs, got %v", err)
	}
}

func TestCSRFValidation(t *testing.T) {
	// GET request: no CSRF check needed
	req := httptest.NewRequest(http.MethodGet, "/hub/api/me", nil)
	if err := ValidateCSRF(req, ""); err != nil {
		t.Fatalf("expected GET to pass CSRF, got %v", err)
	}

	// POST without application/json: rejected
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://hub.local:8380")
	if err := ValidateCSRF(req, ""); err != ErrCSRFContentType {
		t.Fatalf("expected ErrCSRFContentType, got %v", err)
	}

	// POST with application/json but missing Origin and Referer: rejected
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if err := ValidateCSRF(req, ""); err != ErrCSRFMissingOrigin {
		t.Fatalf("expected ErrCSRFMissingOrigin, got %v", err)
	}

	// POST with foreign Origin: rejected
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "hub.local:8380"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.attacker.com")
	if err := ValidateCSRF(req, ""); err != ErrCSRFOriginMismatch {
		t.Fatalf("expected ErrCSRFOriginMismatch, got %v", err)
	}

	// POST with matching Origin: accepted
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "hub.local:8380"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://hub.local:8380")
	if err := ValidateCSRF(req, ""); err != nil {
		t.Fatalf("expected matching Origin to pass, got %v", err)
	}

	// POST with matching Referer when Origin is absent: accepted
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "hub.local:8380"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "http://hub.local:8380/login")
	if err := ValidateCSRF(req, ""); err != nil {
		t.Fatalf("expected matching Referer to pass, got %v", err)
	}

	// POST with configured publicURL: accepted
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "internal-lan:8380"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hub.example.com")
	if err := ValidateCSRF(req, "https://hub.example.com"); err != nil {
		t.Fatalf("expected publicURL Origin to pass, got %v", err)
	}

	// POST with different port on same host: rejected (item 4: host:9999 vs host:8080)
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "hub.local:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://hub.local:9999")
	if err := ValidateCSRF(req, ""); err != ErrCSRFOriginMismatch {
		t.Fatalf("expected different port to be rejected, got %v", err)
	}

	// POST with default port 80 normalized against host without port: accepted
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "hub.local"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://hub.local:80")
	if err := ValidateCSRF(req, ""); err != nil {
		t.Fatalf("expected default port 80 to match host, got %v", err)
	}

	// POST with default port 443 normalized against publicURL: accepted
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "lan:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hub.example.com:443")
	if err := ValidateCSRF(req, "https://hub.example.com"); err != nil {
		t.Fatalf("expected default port 443 to match publicURL, got %v", err)
	}

	// POST with non-default port against publicURL: rejected
	req = httptest.NewRequest(http.MethodPost, "/hub/api/login", strings.NewReader(`{}`))
	req.Host = "lan:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hub.example.com:8443")
	if err := ValidateCSRF(req, "https://hub.example.com"); err != ErrCSRFOriginMismatch {
		t.Fatalf("expected mismatched port on publicURL to be rejected, got %v", err)
	}
}

func TestClientIPAndRateLimitPorts(t *testing.T) {
	// 1. ClientIP port stripping and XFF behavior
	r1 := httptest.NewRequest(http.MethodPost, "/hub/api/login", nil)
	r1.RemoteAddr = "192.168.1.50:12345"
	if ip := ClientIP(r1, ""); ip != "192.168.1.50" {
		t.Fatalf("expected 192.168.1.50, got %s", ip)
	}

	// XFF ignored from non-trusted proxy
	r1.Header.Set("X-Forwarded-For", "203.0.113.195")
	if ip := ClientIP(r1, "127.0.0.1"); ip != "192.168.1.50" {
		t.Fatalf("expected 192.168.1.50, got %s", ip)
	}

	// XFF honored from trusted proxy (last address in chain)
	r2 := httptest.NewRequest(http.MethodPost, "/hub/api/login", nil)
	r2.RemoteAddr = "127.0.0.1:45678"
	r2.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.195:8080")
	if ip := ClientIP(r2, "127.0.0.1"); ip != "203.0.113.195" {
		t.Fatalf("expected 203.0.113.195, got %s", ip)
	}

	// 2. Different ports from same host share rate-limiting bucket
	rl := NewRateLimiter()
	for i := 0; i < LoginMaxFailures; i++ {
		rl.RecordLoginFailure(fmt.Sprintf("user-%d", i), fmt.Sprintf("192.168.1.100:%d", 10000+i))
	}

	// Another connection from the same host on a different port must be locked out
	if rl.LoginAllowed("new-user", "192.168.1.100:9999") {
		t.Fatalf("expected 192.168.1.100:9999 to be locked out due to shared IP bucket")
	}

	// Connection from a different host must still be allowed
	if !rl.LoginAllowed("new-user", "192.168.1.101:9999") {
		t.Fatalf("expected 192.168.1.101:9999 to be allowed")
	}
}

func TestUnknownUserDummyVerification(t *testing.T) {
	_, svc, cleanup := newTestStoreAndAuth(t)
	defer cleanup()

	// Verify that dummyPasswordHash is valid argon2id format and fails verification cleanly
	ok, err := VerifyPassword("arbitrary-password", dummyPasswordHash)
	if err != nil {
		t.Fatalf("expected dummy hash to parse correctly, got err=%v", err)
	}
	if ok {
		t.Fatal("expected dummy hash verification to return false")
	}

	// Login with non-existent user should fail with ErrInvalidCredentials
	_, _, _, err = svc.Login(context.Background(), "nonexistent-user", "password123!", "ua", "127.0.0.1")
	if err != ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestArgon2SemaphoreConcurrencyLimit(t *testing.T) {
	restoreTimeout := SetArgonTimeoutForTest(50 * time.Millisecond)
	defer restoreTimeout()

	// Fill the 2 semaphore slots
	releaseSlots := HoldArgonSlotsForTest(2)

	// Attempting a 3rd hash/verify must fail with ErrBusy
	_, err := HashPassword("ValidPassword123!")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy when semaphore is full, got %v", err)
	}

	_, err = VerifyPassword("ValidPassword123!", dummyPasswordHash)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy when semaphore is full for verify, got %v", err)
	}

	// Release slots: subsequent calls must succeed
	releaseSlots()

	hash, err := HashPassword("ValidPassword123!")
	if err != nil {
		t.Fatalf("expected HashPassword to succeed after releasing slots, got %v", err)
	}
	if hash == "" {
		t.Fatal("empty hash")
	}
}

func TestConcurrentSetupRace(t *testing.T) {
	_, svc, cleanup := newTestStoreAndAuth(t)
	defer cleanup()

	setupCode := svc.SetupCode()
	if setupCode == "" {
		t.Fatal("expected non-empty setup code")
	}

	const n = 5
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			_, _, _, err := svc.Setup(
				context.Background(),
				setupCode,
				fmt.Sprintf("admin%d", idx),
				"Admin",
				"ValidPassword123!",
				"ua",
				fmt.Sprintf("192.168.1.%d", idx+1),
			)
			errs <- err
		}(i)
	}

	successes := 0
	setupNotNeeded := 0
	for i := 0; i < n; i++ {
		err := <-errs
		if err == nil {
			successes++
		} else if errors.Is(err, ErrSetupNotNeeded) || errors.Is(err, ErrInvalidSetupCode) {
			setupNotNeeded++
		} else {
			t.Errorf("unexpected error from concurrent setup: %v", err)
		}
	}

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful setup, got %d", successes)
	}
	if setupNotNeeded != n-1 {
		t.Fatalf("expected %d requests to fail with setup not needed or code consumed, got %d", n-1, setupNotNeeded)
	}
}
