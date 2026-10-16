package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"nexus/pkg/auth"
	"nexus/pkg/push"
	"nexus/pkg/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newPushTestHub(t *testing.T) (*Hub, *httptest.Server, *store.Store, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nexus-push-hub-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "hub.db")
	h, err := New(Config{
		Version:      "test",
		Tokens:       []string{"test-legacy-token"},
		DBPath:       dbPath,
		TrustedProxy: "127.0.0.1",
		PublicURL:    "http://hub.local:8380",
	})
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("New hub: %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	cleanup := func() {
		srv.Close()
		if h.Store() != nil {
			h.Store().Close()
		}
		os.RemoveAll(dir)
	}
	return h, srv, h.Store(), cleanup
}

func createAdminAndLogin(t *testing.T, srv *httptest.Server, st *store.Store, username, password string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	passHash, _ := auth.HashPassword(password)
	admin := &store.User{
		Username:    username,
		DisplayName: "Admin User",
		Role:        "admin",
		PassHash:    passHash,
		MustChange:  false,
	}
	if err := st.CreateUser(ctx, admin); err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}

	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login failed: status %d", res.StatusCode)
	}
	cookie := findCookie(res.Cookies(), auth.SessionCookieName)
	if cookie == nil {
		t.Fatalf("session cookie not found after login")
	}
	return cookie
}

func createMemberAndLogin(t *testing.T, srv *httptest.Server, st *store.Store, username, password string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	passHash, _ := auth.HashPassword(password)
	member := &store.User{
		Username:    username,
		DisplayName: "Member User",
		Role:        "member",
		PassHash:    passHash,
		MustChange:  false,
	}
	if err := st.CreateUser(ctx, member); err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}

	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login failed: status %d", res.StatusCode)
	}
	cookie := findCookie(res.Cookies(), auth.SessionCookieName)
	if cookie == nil {
		t.Fatalf("session cookie not found after login")
	}
	return cookie
}

func createTestDeviceWithToken(t *testing.T, st *store.Store, devID, name string) (rawToken string) {
	t.Helper()
	ctx := context.Background()
	rawToken = "dev-raw-token-" + devID
	tokHash := auth.HashDeviceToken(rawToken)

	dev := &store.Device{
		ID:        devID,
		OwnerID:   1, // will be bound to first user
		Name:      name,
		Kind:      "android",
		TokenHash: tokHash,
	}
	if err := st.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}
	return rawToken
}

func doDeviceRequest(t *testing.T, srv *httptest.Server, method, path, bearerToken string, body any) (*http.Response, []byte) {
	t.Helper()
	var bodyReader bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&bodyReader).Encode(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, &bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil || method == http.MethodPut || method == http.MethodPost || method == http.MethodDelete {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("do device request %s %s: %v", method, path, err)
	}
	resBytes, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, resBytes
}

func TestDeviceSelfPushPutAndDelete(t *testing.T) {
	h, srv, st, cleanup := newPushTestHub(t)
	defer cleanup()

	_ = createAdminAndLogin(t, srv, st, "admin", "admin-pass-1234")
	devID := "phone-alice"
	deviceToken := createTestDeviceWithToken(t, st, devID, "Alice Phone")

	// 1. Auth failure: Missing Authorization header
	res, _ := doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", "", map[string]string{
		"provider": "fcm",
		"token":    "fcm-tok-1",
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing auth, got %d", res.StatusCode)
	}

	// 2. Auth failure: Bad Bearer token
	res, _ = doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", "invalid-device-token", map[string]string{
		"provider": "fcm",
		"token":    "fcm-tok-1",
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on bad token, got %d", res.StatusCode)
	}

	// 3. Validation: Bad provider
	res, _ = doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", deviceToken, map[string]string{
		"provider": "apns",
		"token":    "fcm-tok-1",
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad provider, got %d", res.StatusCode)
	}

	// 4. Validation: Empty token
	res, _ = doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", deviceToken, map[string]string{
		"provider": "fcm",
		"token":    "",
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty token, got %d", res.StatusCode)
	}

	// 5. Validation: Oversized token (>= 4 KB)
	largeToken := strings.Repeat("A", 4096)
	res, _ = doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", deviceToken, map[string]string{
		"provider": "fcm",
		"token":    largeToken,
	})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on token >= 4KB, got %d", res.StatusCode)
	}

	// 6. Successful PUT: upsert token
	validToken := "fcm-device-push-token-12345"
	res, _ = doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", deviceToken, map[string]string{
		"provider": "fcm",
		"token":    validToken,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on valid PUT, got %d", res.StatusCode)
	}

	// Check token stored in database
	pt, err := st.GetPushToken(context.Background(), devID)
	if err != nil {
		t.Fatalf("GetPushToken failed: %v", err)
	}
	if pt.Token != validToken || pt.Provider != "fcm" {
		t.Fatalf("unexpected stored push token: %+v", pt)
	}

	// 7. Successful DELETE
	res, _ = doDeviceRequest(t, srv, http.MethodDelete, "/hub/api/devices/self/push", deviceToken, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on DELETE, got %d", res.StatusCode)
	}

	// Token must be deleted
	if _, err := st.GetPushToken(context.Background(), devID); err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after DELETE, got %v", err)
	}

	// 8. Idempotent DELETE again
	res, _ = doDeviceRequest(t, srv, http.MethodDelete, "/hub/api/devices/self/push", deviceToken, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on second DELETE, got %d", res.StatusCode)
	}
	_ = h
}

func TestDeviceSelfPushRateLimit(t *testing.T) {
	h, srv, st, cleanup := newPushTestHub(t)
	defer cleanup()

	_ = createAdminAndLogin(t, srv, st, "admin", "admin-pass-1234")
	devID := "phone-bob"
	deviceToken := createTestDeviceWithToken(t, st, devID, "Bob Phone")

	// 10 updates allowed per hour
	for i := 1; i <= 10; i++ {
		res, _ := doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", deviceToken, map[string]string{
			"provider": "fcm",
			"token":    fmt.Sprintf("token-%d", i),
		})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected update %d to succeed, got %d", i, res.StatusCode)
		}
	}

	// 11th update within the hour must be rejected with 429 Too Many Requests
	res, body := doDeviceRequest(t, srv, http.MethodPut, "/hub/api/devices/self/push", deviceToken, map[string]string{
		"provider": "fcm",
		"token":    "token-11",
	})
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 11th update, got %d (body: %s)", res.StatusCode, string(body))
	}
	_ = h
}

func TestAdminPushTestEndpoint(t *testing.T) {
	h, srv, st, cleanup := newPushTestHub(t)
	defer cleanup()

	adminCookie := createAdminAndLogin(t, srv, st, "admin", "admin-password")
	memberCookie := createMemberAndLogin(t, srv, st, "member", "member-password")

	devID := "phone-charlie"
	devToken := createTestDeviceWithToken(t, st, devID, "Charlie Phone")
	// Save push token for devID
	_ = st.UpsertPushToken(context.Background(), &store.PushToken{
		DeviceID: devID,
		Provider: "fcm",
		Token:    "fcm-token-charlie",
	})
	_ = devToken

	// 1. CSRF validation test
	reqBody, _ := json.Marshal(map[string]string{"device_id": devID, "t": "call"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/admin/push-test", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.com")
	req.AddCookie(adminCookie)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on CSRF mismatch, got %d", res.StatusCode)
	}

	// 2. Admin-only: unauthenticated
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/admin/push-test", map[string]string{
		"device_id": devID,
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated push-test, got %d", res.StatusCode)
	}

	// 3. Admin-only: non-admin (member) role
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/admin/push-test", map[string]string{
		"device_id": devID,
	}, memberCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for member role, got %d", res.StatusCode)
	}

	// 4. Push-off mode: when pushSender is nil
	h.SetPushSender(nil)
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/admin/push-test", map[string]string{
		"device_id": devID,
	}, adminCookie)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when push is off, got %d (body: %s)", res.StatusCode, string(body))
	}

	// 5. Configured FakeSender: test "no token" on non-registered device
	fake := push.NewFakeSender()
	h.SetPushSender(fake)

	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/admin/push-test", map[string]string{
		"device_id": "phone-no-token",
	}, adminCookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for device without token, got %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "no token") {
		t.Fatalf("expected 'no token' error message, got %s", string(body))
	}

	// 6. Successful push-test
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/admin/push-test", map[string]string{
		"device_id": devID,
		"t":         "call",
	}, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for successful push-test, got %d (body: %s)", res.StatusCode, string(body))
	}

	var respObj map[string]any
	_ = json.Unmarshal(body, &respObj)
	if respObj["ok"] != true || respObj["device_id"] != devID || respObj["t"] != "call" {
		t.Fatalf("unexpected push-test response: %+v", respObj)
	}
	if respObj["call_id"] == "" {
		t.Fatalf("expected non-empty call_id")
	}

	// Verify fake sender received the message
	sent := fake.Sent()
	if len(sent) != 1 || sent[0].DeviceID != devID || sent[0].Message.Type != push.TypeCall {
		t.Fatalf("fake sender did not record expected message: %+v", sent)
	}

	// 7. Unknown message type rejected with 400 Bad Request
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/admin/push-test", map[string]string{
		"device_id": devID,
		"t":         "unsupported_type",
	}, adminCookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown message type, got %d", res.StatusCode)
	}
}

func TestPushOffModeStartup(t *testing.T) {
	// 1. Without credentials, push is off and hub logs it
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dir, err := os.MkdirTemp("", "nexus-push-off-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "hub.db")

	h, err := New(Config{
		Version: "test",
		Tokens:  []string{"dummy"},
		DBPath:  dbPath,
		Log:     logger,
	})
	if err != nil {
		t.Fatalf("New hub failed: %v", err)
	}
	defer h.Store().Close()

	if h.PushSender() != nil {
		t.Fatalf("expected PushSender() to be nil when not configured")
	}
	if !strings.Contains(logBuf.String(), "push off") {
		t.Fatalf("expected 'push off' in log output, got: %s", logBuf.String())
	}

	// 2. With invalid / non-existent credentials path:
	// Acceptance test: "Wrong --fcm-credentials: The hub starts, logs "push off" once, and calls still ring sockets"
	logBuf.Reset()
	h2, err := New(Config{
		Version:        "test",
		Tokens:         []string{"dummy"},
		DBPath:         filepath.Join(dir, "hub2.db"),
		Log:            logger,
		FCMCredentials: "/non/existent/path/fcm.json",
		FCMProject:     "test-project",
	})
	if err != nil {
		t.Fatalf("New hub must not fail when --fcm-credentials is wrong: %v", err)
	}
	defer h2.Store().Close()

	if h2.PushSender() != nil {
		t.Fatalf("expected PushSender() to be nil on invalid credentials")
	}
	if !strings.Contains(logBuf.String(), "push off") {
		t.Fatalf("expected 'push off' in log output on invalid credentials, got: %s", logBuf.String())
	}
}
