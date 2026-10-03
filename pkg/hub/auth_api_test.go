package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"nexus/pkg/auth"
	"nexus/pkg/store"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newAuthTestHub(t *testing.T) (*Hub, *httptest.Server, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nexus-hub-auth-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "hub.db")
	h, err := New(Config{
		Version:      "test",
		Tokens:       []string{testToken},
		DBPath:       dbPath,
		TrustedProxy: "127.0.0.1",
		PublicURL:    "http://hub.local:8380",
	})
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	cleanup := func() {
		srv.Close()
		if h.Store() != nil {
			h.Store().Close()
		}
		os.RemoveAll(dir)
	}
	return h, srv, cleanup
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, body any, cookie *http.Cookie) (*http.Response, []byte) {
	t.Helper()
	var bodyReader bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&bodyReader).Encode(body); err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &bodyReader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", srv.URL)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %v", method, path, err)
	}
	resBody := new(bytes.Buffer)
	_, _ = resBody.ReadFrom(res.Body)
	res.Body.Close()
	return res, resBody.Bytes()
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestHubAuthEndpointsWorkflow(t *testing.T) {
	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// 1. Initial setup check: setup should be needed
	res, body := doJSON(t, srv, http.MethodGet, "/hub/api/setup", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /hub/api/setup: got status %d", res.StatusCode)
	}
	var setupStatus map[string]bool
	_ = json.Unmarshal(body, &setupStatus)
	if !setupStatus["needed"] {
		t.Fatalf("expected needed: true, got %v", setupStatus)
	}

	setupCode := h.Auth().SetupCode()
	if setupCode == "" {
		t.Fatal("expected non-empty setup code")
	}

	// 2. Setup with invalid code: 400 Bad Request
	badSetup := map[string]string{
		"setup_code":   "999-999",
		"username":     "admin",
		"display_name": "Admin User",
		"password":     "AdminPassword123!",
	}
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/setup", badSetup, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /hub/api/setup with bad code: got status %d", res.StatusCode)
	}

	// 3. Setup with valid code: creates admin and returns session cookie
	validSetup := map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin User",
		"password":     "AdminPassword123!",
	}
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/setup", validSetup, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /hub/api/setup valid: got status %d, body: %s", res.StatusCode, string(body))
	}
	cookie := findCookie(res.Cookies(), auth.SessionCookieName)
	if cookie == nil || cookie.Value == "" {
		t.Fatal("expected nexus_session cookie on setup")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected cookie attributes: HttpOnly=%v, SameSite=%v", cookie.HttpOnly, cookie.SameSite)
	}

	// 4. GET /hub/api/setup now needed: false
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/setup", nil, nil)
	_ = json.Unmarshal(body, &setupStatus)
	if setupStatus["needed"] {
		t.Fatalf("expected needed: false after setup, got %v", setupStatus)
	}

	// 5. GET /hub/api/me with session
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/me", nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /hub/api/me: got status %d, body: %s", res.StatusCode, string(body))
	}
	var me map[string]any
	_ = json.Unmarshal(body, &me)
	if me["username"] != "admin" || me["role"] != "admin" {
		t.Fatalf("unexpected /me response: %v", me)
	}

	// 6. GET /hub/api/me without session: 401
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/api/me", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /hub/api/me unauthenticated: got status %d", res.StatusCode)
	}

	// 7. Admin creates member user: POST /hub/api/users
	newUserReq := map[string]string{
		"username":     "riya",
		"display_name": "Riya",
		"role":         "member",
	}
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/users", newUserReq, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /hub/api/users: got status %d, body: %s", res.StatusCode, string(body))
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	tempPass := created["temp_password"].(string)
	if tempPass == "" {
		t.Fatal("expected temp_password in create response")
	}
	userData := created["user"].(map[string]any)
	riyaID := int64(userData["id"].(float64))

	// 8. Admin lists users: GET /hub/api/users
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/users", nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /hub/api/users: got status %d", res.StatusCode)
	}
	var usersList []map[string]any
	_ = json.Unmarshal(body, &usersList)
	if len(usersList) != 2 {
		t.Fatalf("expected 2 users in list, got %d", len(usersList))
	}

	// 9. Member logs in: POST /hub/api/login
	loginReq := map[string]string{
		"username": "riya",
		"password": tempPass,
	}
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/login", loginReq, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /hub/api/login for member: got status %d, body: %s", res.StatusCode, string(body))
	}
	memberCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	if memberCookie == nil {
		t.Fatal("expected session cookie for member login")
	}

	// 10. Member cannot list users (admin required): 403 Forbidden
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/api/users", nil, memberCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for member on GET /hub/api/users, got %d", res.StatusCode)
	}

	// 11. Member changes password: POST /hub/api/me/password
	pwChangeReq := map[string]string{
		"current": tempPass,
		"new":     "NewPassword123!",
	}
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", pwChangeReq, memberCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /hub/api/me/password: got status %d, body: %s", res.StatusCode, string(body))
	}

	// 12. Admin resets user password: POST /hub/api/users/{id}/reset
	res, body = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/reset", riyaID), map[string]any{}, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /hub/api/users/{id}/reset: got status %d, body: %s", res.StatusCode, string(body))
	}
	var resetRes map[string]string
	_ = json.Unmarshal(body, &resetRes)
	newTemp := resetRes["temp_password"]
	if newTemp == "" {
		t.Fatal("expected temp_password in reset response")
	}

	// 13. Member's old session was revoked by reset
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/api/me", nil, memberCookie)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected member old session unauthorized after reset, got %d", res.StatusCode)
	}

	// 14. Admin deletes user: DELETE /hub/api/users/{id}
	// Cannot delete self:
	res, _ = doJSON(t, srv, http.MethodDelete, fmt.Sprintf("/hub/api/users/%d", int64(me["id"].(float64))), nil, cookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when deleting self, got %d", res.StatusCode)
	}

	// Delete riya:
	res, _ = doJSON(t, srv, http.MethodDelete, fmt.Sprintf("/hub/api/users/%d", riyaID), nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /hub/api/users/{id}: got status %d", res.StatusCode)
	}

	// 15. Logout: POST /hub/api/logout
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/logout", nil, cookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /hub/api/logout: got status %d", res.StatusCode)
	}
	clearedCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	if clearedCookie == nil || clearedCookie.MaxAge != -1 {
		t.Fatalf("expected cleared cookie with MaxAge: -1, got %+v", clearedCookie)
	}

	// Old session is now unauthorized
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/api/me", nil, cookie)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", res.StatusCode)
	}
}

func TestAuthMiddlewareOnExistingRoutes(t *testing.T) {
	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Setup admin
	setupCode := h.Auth().SetupCode()
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin",
		"password":     "AdminSecret123!",
	}, nil)
	adminCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// Create member user
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     "member1",
		"display_name": "Member 1",
		"role":         "member",
	}, adminCookie)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	tempPass := created["temp_password"].(string)

	// Login member
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "member1",
		"password": tempPass,
	}, nil)
	memberCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// Member must change temporary password to clear must_change restriction
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", map[string]string{
		"current": tempPass,
		"new":     "NewMemberSecret123!",
	}, memberCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /hub/api/me/password, got %d", res.StatusCode)
	}

	// --- /hub/status ---
	// Unauthenticated: 401
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/status", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /hub/status unauthenticated, got %d", res.StatusCode)
	}
	// Member: 403 Forbidden
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/status", nil, memberCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on /hub/status for member, got %d", res.StatusCode)
	}
	// Admin: 200 OK
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/status", nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /hub/status for admin, got %d", res.StatusCode)
	}

	// --- /api/* proxy ---
	// Unauthenticated: 401
	res, _ = doJSON(t, srv, http.MethodGet, "/api/alarms", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/alarms unauthenticated, got %d", res.StatusCode)
	}
	// Authenticated: authorized (returns 503 board offline since no board connected)
	res, _ = doJSON(t, srv, http.MethodGet, "/api/alarms", nil, memberCookie)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 board offline on /api/alarms when authenticated, got %d", res.StatusCode)
	}

	// --- /api/ws browser WebSocket ---
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	// Unauthenticated dial: 401 error
	_, _, err := websocket.Dial(ctx, wsBase+"/api/ws", nil)
	if err == nil {
		t.Fatal("expected dial error without session cookie on /api/ws")
	}

	// Authenticated dial: passes auth check (fails with closeBoardOffline because no board connected)
	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{memberCookie.String()},
			"Origin": []string{srv.URL},
		},
	}
	conn, _, err := websocket.Dial(ctx, wsBase+"/api/ws", opts)
	if err != nil {
		t.Fatalf("expected authenticated dial to connect, got err=%v", err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != closeBoardOffline {
		t.Fatalf("expected closeBoardOffline after auth passed, got %v", err)
	}
}

func TestCSRFProtection(t *testing.T) {
	_, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// 1. Missing Content-Type on POST: 403
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/setup", strings.NewReader(`{}`))
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without Content-Type: application/json, got %d", res.StatusCode)
	}

	// 2. Foreign Origin: 403
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/hub/api/setup", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://attacker.site")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with foreign Origin, got %d", res.StatusCode)
	}

	// 3. Matching Origin: passes CSRF check (fails later on request validation e.g. 400 Bad Request)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/hub/api/setup", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode == http.StatusForbidden {
		t.Fatalf("expected matching Origin to pass CSRF, got 403")
	}
}

func TestFailClosedUnopenableDB(t *testing.T) {
	// An unopenable path (inside a file treated as directory)
	unopenable := "/dev/null/unopenable.db"
	_, err := New(Config{
		Version: "test",
		Tokens:  []string{testToken},
		DBPath:  unopenable,
	})
	if err == nil {
		t.Fatal("expected error on unopenable DB path, got nil")
	}
}

func TestFailClosedNoAuthWithoutFlag(t *testing.T) {
	// When neither Auth/Store/DBPath nor NoAuth is set, New must fail closed
	_, err := New(Config{
		Version: "test",
		Tokens:  []string{testToken},
	})
	if err == nil {
		t.Fatal("expected error when auth is not configured, got nil")
	}
}

func TestBrowserWSOriginValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir, err := os.MkdirTemp("", "nexus-ws-origin-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "hub.db")
	h, err := New(Config{
		Version:        "test",
		Tokens:         []string{testToken},
		DBPath:         dbPath,
		TrustedProxy:   "127.0.0.1",
		PublicURL:      "http://hub.local:8380",
		AllowedOrigins: []string{"http://localhost:5173", "http://dev.local:3000"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// 1. Initial setup to get a valid session cookie
	setupBody := `{"setup_code":"` + h.auth.SetupCode() + `","username":"admin","display_name":"Admin","password":"admin-password123"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/setup", strings.NewReader(setupBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var sessCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			sessCookie = c
			break
		}
	}
	if sessCookie == nil {
		t.Fatal("expected session cookie from setup")
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"

	// 2. Request with session cookie but MISSING Origin: rejected (403)
	optsMissingOrigin := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{sessCookie.String()},
		},
	}
	_, _, err = websocket.Dial(ctx, wsURL, optsMissingOrigin)
	if err == nil {
		t.Fatal("expected error when connecting with session cookie and missing Origin")
	}

	// 3. Request with session cookie and WRONG Origin: rejected (403)
	optsWrongOrigin := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{sessCookie.String()},
			"Origin": []string{"http://attacker.com"},
		},
	}
	_, _, err = websocket.Dial(ctx, wsURL, optsWrongOrigin)
	if err == nil {
		t.Fatal("expected error when connecting with wrong Origin")
	}

	// 4. Request with session cookie and matching Host Origin: accepted
	optsValidHostOrigin := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{sessCookie.String()},
			"Origin": []string{srv.URL},
		},
	}
	conn, _, err := websocket.Dial(ctx, wsURL, optsValidHostOrigin)
	if err != nil {
		t.Fatalf("expected valid Host Origin to connect, got %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 5. Request with session cookie and matching --allow-origin: accepted
	optsAllowedOrigin := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{sessCookie.String()},
			"Origin": []string{"http://localhost:5173"},
		},
	}
	conn, _, err = websocket.Dial(ctx, wsURL, optsAllowedOrigin)
	if err != nil {
		t.Fatalf("expected --allow-origin to connect, got %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 6. Non-browser client without cookie and without Origin on test hub with NoAuth: accepted
	hNoAuth, err := New(Config{Version: "test", Tokens: []string{testToken}, NoAuth: true})
	if err != nil {
		t.Fatalf("New NoAuth: %v", err)
	}
	srvNoAuth := httptest.NewServer(hNoAuth.Handler())
	defer srvNoAuth.Close()

	wsURLNoAuth := "ws" + strings.TrimPrefix(srvNoAuth.URL, "http") + "/api/ws"
	conn, _, err = websocket.Dial(ctx, wsURLNoAuth, nil) // no cookie, no origin
	if err != nil {
		t.Fatalf("expected non-browser client without cookie and origin to connect to NoAuth hub, got %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func TestHubAPIBodySizeLimit(t *testing.T) {
	_, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// Payload > 16 KiB (e.g. 20 KiB): rejected with 413 Request Entity Too Large
	largePayload := `{"username":"` + strings.Repeat("a", 20*1024) + `","password":"foo"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/login", strings.NewReader(largePayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 Request Entity Too Large for >16 KiB body, got %d", res.StatusCode)
	}
}

func TestArgon2SemaphoreHTTP503(t *testing.T) {
	_, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	restoreTimeout := auth.SetArgonTimeoutForTest(50 * time.Millisecond)
	defer restoreTimeout()

	// Fill both argon semaphore slots
	releaseSlots := auth.HoldArgonSlotsForTest(2)
	defer releaseSlots()

	// Attempting login while busy must return 503 Service Unavailable
	reqBody := `{"username":"admin","password":"password123!"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/login", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when argon2 is busy, got %d", res.StatusCode)
	}
}

func TestAPIProxyWritesOriginCheck(t *testing.T) {
	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// Initial setup to get valid session cookie
	setupBody := `{"setup_code":"` + h.auth.SetupCode() + `","username":"admin","display_name":"Admin","password":"admin-password123"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/setup", strings.NewReader(setupBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var sessCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			sessCookie = c
			break
		}
	}
	if sessCookie == nil {
		t.Fatal("expected session cookie from setup")
	}

	// 1. GET /api/status without Origin: allowed through (no CSRF check for GET), returns 503 board offline
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/status", nil)
	req.AddCookie(sessCookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status failed: %v", err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for GET without Origin, got %d", res.StatusCode)
	}

	// 2. POST /api/ota with session cookie but MISSING Origin and Referer: 403 Forbidden
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/ota", strings.NewReader("binary-data"))
	req.AddCookie(sessCookie)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/ota failed: %v", err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for POST without Origin/Referer, got %d", res.StatusCode)
	}

	// 3. POST /api/ota with session cookie and MISMATCHED Origin: 403 Forbidden
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/ota", strings.NewReader("binary-data"))
	req.AddCookie(sessCookie)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", "http://evil.com")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/ota failed: %v", err)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for POST with mismatched Origin, got %d", res.StatusCode)
	}

	// 4. POST /api/ota with session cookie, non-JSON Content-Type, and MATCHING Origin: accepted (proceeds to proxy, returning 503 board offline)
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/ota", strings.NewReader("binary-data"))
	req.AddCookie(sessCookie)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", srv.URL)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/ota failed: %v", err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 board offline for valid POST /api/ota, got %d", res.StatusCode)
	}
}

func TestMustChangeEnforcement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// Initial setup
	setupBody := `{"setup_code":"` + h.auth.SetupCode() + `","username":"admin","display_name":"Admin","password":"admin-password123"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/hub/api/setup", strings.NewReader(setupBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	var adminCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			adminCookie = c
			break
		}
	}

	// Create user with must_change: true
	createReq := `{"username":"newuser","role":"member"}`
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/hub/api/users", strings.NewReader(createReq))
	req.AddCookie(adminCookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	var createResp struct {
		User         store.User `json:"user"`
		TempPassword string     `json:"temp_password"`
	}
	json.NewDecoder(res.Body).Decode(&createResp)
	res.Body.Close()

	if !createResp.User.MustChange {
		t.Fatal("expected newUser.MustChange to be true")
	}

	// Login as newuser
	loginReq := `{"username":"newuser","password":"` + createResp.TempPassword + `"}`
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/hub/api/login", strings.NewReader(loginReq))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("login as new user: %v", err)
	}
	var userCookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			userCookie = c
			break
		}
	}
	res.Body.Close()
	if userCookie == nil {
		t.Fatal("missing session cookie for newuser")
	}

	// 1. GET /hub/api/me is allowed
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/hub/api/me", nil)
	req.AddCookie(userCookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("expected GET /hub/api/me to return 200, got %v status=%d", err, res.StatusCode)
	}
	res.Body.Close()

	// 2. GET /api/status returns 403 {"error":"password change required"}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/status", nil)
	req.AddCookie(userCookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected GET /api/status to return 403, got %v status=%d", err, res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "password change required") {
		t.Fatalf("expected error 'password change required', got %s", string(body))
	}

	// 3. WS /api/ws returns 403
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"
	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Cookie": []string{userCookie.String()},
			"Origin": []string{srv.URL},
		},
	}
	_, _, err = websocket.Dial(ctx, wsURL, opts)
	if err == nil {
		t.Fatal("expected websocket connection to be rejected when must_change is true")
	}

	// 4. POST /hub/api/me/password to change password is allowed
	changeBody := `{"current":"` + createResp.TempPassword + `","new":"BrandNewPassword123!"}`
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/hub/api/me/password", strings.NewReader(changeBody))
	req.AddCookie(userCookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("expected POST /hub/api/me/password to succeed, got %v status=%d", err, res.StatusCode)
	}
	res.Body.Close()

	// 5. Now GET /api/status passes must_change check (returns 503 board offline instead of 403)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/status", nil)
	req.AddCookie(userCookie)
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected GET /api/status to return 503 board offline after password change, got %v status=%d", err, res.StatusCode)
	}
	res.Body.Close()
}

func boolPtr(b bool) *bool {
	return &b
}

func TestPairingCodesAPI(t *testing.T) {
	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// 1. Setup admin
	setupCode := h.Auth().SetupCode()
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin",
		"password":     "AdminSecret123!",
	}, nil)
	adminCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// Create Alice (member)
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     "alice",
		"display_name": "Alice",
		"role":         "member",
	}, adminCookie)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	tempPass := created["temp_password"].(string)

	// Login Alice and change temp password
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "alice",
		"password": tempPass,
	}, nil)
	aliceCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", map[string]string{
		"current": tempPass,
		"new":     "AliceSecret123!",
	}, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /hub/api/me/password, got %d", res.StatusCode)
	}

	// 2. POST /hub/api/pairing unauthenticated -> 401
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", res.StatusCode)
	}

	// 3. Alice creates first pairing code
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /hub/api/pairing, got %d", res.StatusCode)
	}
	var pc1 map[string]any
	_ = json.Unmarshal(body, &pc1)
	code1 := pc1["code"].(string)
	if len(code1) != 7 || code1[3] != '-' {
		t.Fatalf("unexpected code format: %s", code1)
	}

	// Create 2 more codes (total 3)
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on 2nd pairing code, got %d", res.StatusCode)
	}
	var pc2 map[string]any
	_ = json.Unmarshal(body, &pc2)
	code2 := pc2["code"].(string)

	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on 3rd pairing code, got %d", res.StatusCode)
	}

	// 4. Try to create 4th code -> 400 Bad Request
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on 4th pairing code, got %d", res.StatusCode)
	}

	// 5. GET /hub/api/pairing -> returns 3 active codes
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on GET /hub/api/pairing, got %d", res.StatusCode)
	}
	var list []map[string]any
	_ = json.Unmarshal(body, &list)
	if len(list) != 3 {
		t.Fatalf("expected 3 active codes, got %d", len(list))
	}

	// 6. DELETE /hub/api/pairing/{code} to cancel code2
	res, _ = doJSON(t, srv, http.MethodDelete, "/hub/api/pairing/"+code2, nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on DELETE /hub/api/pairing/{code}, got %d", res.StatusCode)
	}

	// Now Alice has 2 active codes, can create 1 more
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating replacement code, got %d", res.StatusCode)
	}

	// 7. DELETE /hub/api/pairing -> cancels all active codes
	res, _ = doJSON(t, srv, http.MethodDelete, "/hub/api/pairing", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on DELETE /hub/api/pairing, got %d", res.StatusCode)
	}
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/pairing", nil, aliceCookie)
	_ = json.Unmarshal(body, &list)
	if len(list) != 0 {
		t.Fatalf("expected 0 active codes after cancel all, got %d", len(list))
	}
}

func TestProto3EdgePairingAndLifecycle(t *testing.T) {
	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Setup admin
	setupCode := h.Auth().SetupCode()
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin",
		"password":     "AdminSecret123!",
	}, nil)
	adminCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// Create Alice (member)
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     "alice",
		"display_name": "Alice",
		"role":         "member",
	}, adminCookie)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	tempPass := created["temp_password"].(string)

	// Login Alice & update password
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "alice",
		"password": tempPass,
	}, nil)
	aliceCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	_, _ = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", map[string]string{
		"current": tempPass,
		"new":     "AliceSecret123!",
	}, aliceCookie)

	// Alice gets pairing code
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	var pData map[string]any
	_ = json.Unmarshal(body, &pData)
	pairCode := pData["code"].(string)

	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath

	// 2. Case 3: Neither token nor pair_code -> rejected with "pairing required"
	conn, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3"})
	msg := read(t, ctx, conn)
	if msg["type"] != "rejected" || msg["reason"] != "pairing required" {
		t.Fatalf("expected rejected pairing required, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 3. Case 2: Wrong pair_code -> rejected with "bad code"
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3", "pair_code": "000-000"})
	msg = read(t, ctx, conn)
	if msg["type"] != "rejected" || msg["reason"] != "bad code" {
		t.Fatalf("expected rejected bad code, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 4. Case 2: Valid pair_code -> claimed + welcome
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3", "pair_code": pairCode})
	claimedMsg := read(t, ctx, conn)
	if claimedMsg["type"] != "claimed" || claimedMsg["owner"] != "Alice" || claimedMsg["token"] == "" {
		t.Fatalf("expected claimed message with token and owner Alice, got %+v", claimedMsg)
	}
	deviceToken := claimedMsg["token"].(string)

	welcomeMsg := read(t, ctx, conn)
	if welcomeMsg["type"] != "welcome" || welcomeMsg["claimed"] != true || welcomeMsg["owner"] != "Alice" {
		t.Fatalf("expected welcome message with claimed:true and owner:Alice, got %+v", welcomeMsg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 5. Code used once: same pairing code cannot be used again
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-2", "name": "Living Room", "kind": "esp32-s3", "pair_code": pairCode})
	msg = read(t, ctx, conn)
	if msg["type"] != "rejected" || msg["reason"] != "bad code" {
		t.Fatalf("expected rejected bad code on re-use of consumed code, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 6. Case 1: Reconnect with device token
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3", "token": deviceToken})
	msg = read(t, ctx, conn)
	if msg["type"] != "welcome" || msg["claimed"] != true || msg["owner"] != "Alice" {
		t.Fatalf("expected welcome on reconnect with token, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 7. Token bound to ID: replaying token with different ID -> rejected with "bad token"
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-impostor", "name": "Kitchen", "kind": "esp32-s3", "token": deviceToken})
	msg = read(t, ctx, conn)
	if msg["type"] != "rejected" || msg["reason"] != "bad token" {
		t.Fatalf("expected rejected bad token when token replayed with different id, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// 8. Re-claim moves ownership to Bob
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     "bob",
		"display_name": "Bob",
		"role":         "member",
	}, adminCookie)
	_ = json.Unmarshal(body, &created)
	bobTemp := created["temp_password"].(string)

	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "bob",
		"password": bobTemp,
	}, nil)
	bobCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	_, _ = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", map[string]string{
		"current": bobTemp,
		"new":     "BobSecret123!",
	}, bobCookie)

	// Bob creates pairing code
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, bobCookie)
	_ = json.Unmarshal(body, &pData)
	bobCode := pData["code"].(string)

	// Re-claim nexus-dev-1 with Bob's code
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3", "pair_code": bobCode})
	claimedBob := read(t, ctx, conn)
	if claimedBob["type"] != "claimed" || claimedBob["owner"] != "Bob" {
		t.Fatalf("expected claimed with owner Bob, got %+v", claimedBob)
	}
	bobToken := claimedBob["token"].(string)
	welcomeBob := read(t, ctx, conn)
	if welcomeBob["type"] != "welcome" || welcomeBob["owner"] != "Bob" {
		t.Fatalf("expected welcome with owner Bob, got %+v", welcomeBob)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// Old Alice token is now rejected
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3", "token": deviceToken})
	msg = read(t, ctx, conn)
	if msg["type"] != "rejected" || msg["reason"] != "bad token" {
		t.Fatalf("expected old token rejected after re-claim, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	// New Bob token works
	conn, _, err = websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, conn, map[string]any{"type": "hello", "proto": 3, "id": "nexus-dev-1", "name": "Kitchen", "kind": "esp32-s3", "token": bobToken})
	msg = read(t, ctx, conn)
	if msg["type"] != "welcome" || msg["owner"] != "Bob" {
		t.Fatalf("expected new token to work after re-claim, got %+v", msg)
	}
	conn.Close(websocket.StatusNormalClosure, "")
}

func TestDeviceEndpointsAndProxyOwnership(t *testing.T) {
	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Setup admin
	setupCode := h.Auth().SetupCode()
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin",
		"password":     "AdminSecret123!",
	}, nil)
	adminCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// Create Alice & Bob
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     "alice",
		"display_name": "Alice",
		"role":         "member",
	}, adminCookie)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	aliceTemp := created["temp_password"].(string)

	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{"username": "alice", "password": aliceTemp}, nil)
	aliceCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	_, _ = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", map[string]string{"current": aliceTemp, "new": "AliceSecret123!"}, aliceCookie)

	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     "bob",
		"display_name": "Bob",
		"role":         "member",
	}, adminCookie)
	_ = json.Unmarshal(body, &created)
	bobTemp := created["temp_password"].(string)

	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{"username": "bob", "password": bobTemp}, nil)
	bobCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	_, _ = doJSON(t, srv, http.MethodPost, "/hub/api/me/password", map[string]string{"current": bobTemp, "new": "BobSecret123!"}, bobCookie)

	// Alice pairs nexus-alice-1
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, aliceCookie)
	var pData map[string]any
	_ = json.Unmarshal(body, &pData)
	aliceCode := pData["code"].(string)

	// Mock board HTTP server
	fakeBoardServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"board":"ok"}`))
	}))
	defer fakeBoardServer.Close()
	fakeBoardPort, _ := strconv.Atoi(fakeBoardServer.URL[strings.LastIndex(fakeBoardServer.URL, ":")+1:])

	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath
	connAliceBoard, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connAliceBoard.Close(websocket.StatusNormalClosure, "")
	write(t, ctx, connAliceBoard, map[string]any{
		"type":      "hello",
		"proto":     3,
		"id":        "nexus-alice-1",
		"name":      "Alice Speaker",
		"kind":      "esp32-s3",
		"pair_code": aliceCode,
		"roles": map[string]any{
			"state": map[string]any{"http_port": fakeBoardPort},
		},
	})
	claimedAlice := read(t, ctx, connAliceBoard)
	aliceToken := claimedAlice["token"].(string)
	_ = read(t, ctx, connAliceBoard) // welcome

	// Bob pairs nexus-bob-1
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, bobCookie)
	_ = json.Unmarshal(body, &pData)
	bobCode := pData["code"].(string)

	connBobBoard, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connBobBoard.Close(websocket.StatusNormalClosure, "")
	write(t, ctx, connBobBoard, map[string]any{
		"type":      "hello",
		"proto":     3,
		"id":        "nexus-bob-1",
		"name":      "Bob Speaker",
		"kind":      "esp32-s3",
		"pair_code": bobCode,
	})
	_ = read(t, ctx, connBobBoard) // claimed
	_ = read(t, ctx, connBobBoard) // welcome

	// 2. GET /hub/api/devices: Alice sees only hers, Bob sees only his, Admin sees all
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/devices", nil, aliceCookie)
	var aliceDevs []map[string]any
	_ = json.Unmarshal(body, &aliceDevs)
	if len(aliceDevs) != 1 || aliceDevs[0]["id"] != "nexus-alice-1" {
		t.Fatalf("Alice should only see nexus-alice-1, got %+v", aliceDevs)
	}

	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/devices", nil, bobCookie)
	var bobDevs []map[string]any
	_ = json.Unmarshal(body, &bobDevs)
	if len(bobDevs) != 1 || bobDevs[0]["id"] != "nexus-bob-1" {
		t.Fatalf("Bob should only see nexus-bob-1, got %+v", bobDevs)
	}

	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/devices", nil, adminCookie)
	var adminDevs []map[string]any
	_ = json.Unmarshal(body, &adminDevs)
	if len(adminDevs) != 2 {
		t.Fatalf("Admin should see both devices, got %+v", adminDevs)
	}

	// 3. PATCH /hub/api/devices/{id}
	// Bob tries to rename Alice's device -> 403 Forbidden
	res, _ = doJSON(t, srv, http.MethodPatch, "/hub/api/devices/nexus-alice-1", map[string]string{"name": "Hacked"}, bobCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when non-owner renames device, got %d", res.StatusCode)
	}

	// Alice renames her device -> 200 OK
	res, body = doJSON(t, srv, http.MethodPatch, "/hub/api/devices/nexus-alice-1", map[string]string{"name": "Kitchen Smart Speaker"}, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when owner renames device, got %d", res.StatusCode)
	}

	// 4. Proxy /hub/d/{id}/api/...
	// Bob requests Alice's board API -> 403 Forbidden
	res, _ = doJSON(t, srv, http.MethodGet, "/hub/d/nexus-alice-1/api/state", nil, bobCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-owner accessing /hub/d/nexus-alice-1/api/state, got %d", res.StatusCode)
	}

	// Alice requests her board API -> 200 OK {"board":"ok"}
	res, body = doJSON(t, srv, http.MethodGet, "/hub/d/nexus-alice-1/api/state", nil, aliceCookie)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"board":"ok"`) {
		t.Fatalf("expected 200 with board ok for owner, got status=%d body=%s", res.StatusCode, string(body))
	}

	// 5. Existing /api/ proxy and /api/ws (nexus-alice-1 is h.board)
	// Bob requests /api/state -> 403 Forbidden
	res, _ = doJSON(t, srv, http.MethodGet, "/api/state", nil, bobCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-owner accessing /api/state, got %d", res.StatusCode)
	}

	// Alice requests /api/state -> 200 OK
	res, _ = doJSON(t, srv, http.MethodGet, "/api/state", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for owner accessing /api/state, got %d", res.StatusCode)
	}

	// 6. DELETE /hub/api/devices/{id}
	// Bob tries to delete Alice's device -> 403 Forbidden
	res, _ = doJSON(t, srv, http.MethodDelete, "/hub/api/devices/nexus-alice-1", nil, bobCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-owner deleting device, got %d", res.StatusCode)
	}

	// Alice deletes her device -> 200 OK
	res, _ = doJSON(t, srv, http.MethodDelete, "/hub/api/devices/nexus-alice-1", nil, aliceCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for owner deleting device, got %d", res.StatusCode)
	}

	// Reconnect with deleted device's token -> rejected with "bad token"
	connReconnect, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	write(t, ctx, connReconnect, map[string]any{"type": "hello", "proto": 3, "id": "nexus-alice-1", "name": "Kitchen", "kind": "esp32-s3", "token": aliceToken})
	msg := read(t, ctx, connReconnect)
	if msg["type"] != "rejected" || msg["reason"] != "bad token" {
		t.Fatalf("expected rejected bad token for deleted device token, got %+v", msg)
	}
	connReconnect.Close(websocket.StatusNormalClosure, "")
}

func TestLegacyTokensFlag(t *testing.T) {
	ctx := context.Background()

	// 1. Hub with default LegacyTokens (on)
	dir1, err := os.MkdirTemp("", "legacy-hub-1-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir1)

	h1, err := New(Config{
		Version:      "test",
		Tokens:       []string{testToken},
		DBPath:       filepath.Join(dir1, "hub.db"),
		TrustedProxy: "127.0.0.1",
		PublicURL:    "http://hub.local:8380",
		LegacyTokens: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("New h1: %v", err)
	}
	srv1 := httptest.NewServer(h1.Handler())
	defer srv1.Close()
	defer h1.Store().Close()

	// Setup admin on h1 so legacy edge has an owner
	setupCode := h1.Auth().SetupCode()
	_, _ = doJSON(t, srv1, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin User",
		"password":     "AdminSecret123!",
	}, nil)

	// Proto 2 hello with shared token -> receives welcome with legacy: true
	wsURL1 := "ws" + strings.TrimPrefix(srv1.URL, "http") + EdgePath
	conn1, _, err := websocket.Dial(ctx, wsURL1, nil)
	if err != nil {
		t.Fatalf("dial h1: %v", err)
	}
	write(t, ctx, conn1, map[string]any{"type": "hello", "proto": 2, "id": "legacy-dev-1", "name": "Legacy Orbit", "kind": "orbit", "token": testToken})
	msg1 := read(t, ctx, conn1)
	if msg1["type"] != "welcome" || msg1["legacy"] != true {
		t.Fatalf("expected welcome with legacy:true on h1, got %+v", msg1)
	}
	conn1.Close(websocket.StatusNormalClosure, "")

	// 2. Hub with DisableLegacyTokens = true
	dir2, err := os.MkdirTemp("", "legacy-hub-2-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir2)

	h2, err := New(Config{
		Version:             "test",
		Tokens:              []string{testToken},
		DBPath:              filepath.Join(dir2, "hub.db"),
		TrustedProxy:        "127.0.0.1",
		PublicURL:           "http://hub.local:8380",
		DisableLegacyTokens: true,
	})
	if err != nil {
		t.Fatalf("New h2: %v", err)
	}
	srv2 := httptest.NewServer(h2.Handler())
	defer srv2.Close()
	defer h2.Store().Close()

	// Setup admin on h2
	setupCode2 := h2.Auth().SetupCode()
	_, _ = doJSON(t, srv2, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode2,
		"username":     "admin",
		"display_name": "Admin User",
		"password":     "AdminSecret123!",
	}, nil)

	// Proto 2 hello with shared token on h2 -> rejected with "bad token"
	wsURL2 := "ws" + strings.TrimPrefix(srv2.URL, "http") + EdgePath
	conn2, _, err := websocket.Dial(ctx, wsURL2, nil)
	if err != nil {
		t.Fatalf("dial h2: %v", err)
	}
	write(t, ctx, conn2, map[string]any{"type": "hello", "proto": 2, "id": "legacy-dev-1", "name": "Legacy Orbit", "kind": "orbit", "token": testToken})
	msg2 := read(t, ctx, conn2)
	if msg2["type"] != "rejected" || msg2["reason"] != "bad token" {
		t.Fatalf("expected rejected bad token on h2 with legacy disabled, got %+v", msg2)
	}
	conn2.Close(websocket.StatusNormalClosure, "")
}

func TestConcurrentPairingClaims(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, srv, cleanup := newAuthTestHub(t)
	defer cleanup()

	// 1. Setup admin
	setupCode := h.Auth().SetupCode()
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/setup", map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin",
		"password":     "AdminSecret123!",
	}, nil)
	adminCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// Admin requests a pairing code
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("pairing code creation failed: %d %s", res.StatusCode, string(body))
	}
	var pData map[string]any
	_ = json.Unmarshal(body, &pData)
	pairCode := pData["code"].(string)

	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath

	conn1, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close(websocket.StatusNormalClosure, "")

	conn2, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close(websocket.StatusNormalClosure, "")

	readMsg := func(c *websocket.Conn) (map[string]any, error) {
		_, data, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		err = json.Unmarshal(data, &m)
		return m, err
	}

	writeMsg := func(c *websocket.Conn, v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return c.Write(ctx, websocket.MessageText, data)
	}

	type claimResult struct {
		connID   string
		firstMsg map[string]any
		err      error
	}
	results := make(chan claimResult, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	startGate := make(chan struct{})

	go func() {
		defer wg.Done()
		<-startGate
		err := writeMsg(conn1, map[string]any{
			"type":      "hello",
			"proto":     3,
			"id":        "dev-conc-1",
			"name":      "Device One",
			"kind":      "esp32-s3",
			"pair_code": pairCode,
		})
		if err != nil {
			results <- claimResult{connID: "1", err: err}
			return
		}
		msg, err := readMsg(conn1)
		results <- claimResult{connID: "1", firstMsg: msg, err: err}
	}()

	go func() {
		defer wg.Done()
		<-startGate
		err := writeMsg(conn2, map[string]any{
			"type":      "hello",
			"proto":     3,
			"id":        "dev-conc-2",
			"name":      "Device Two",
			"kind":      "esp32-s3",
			"pair_code": pairCode,
		})
		if err != nil {
			results <- claimResult{connID: "2", err: err}
			return
		}
		msg, err := readMsg(conn2)
		results <- claimResult{connID: "2", firstMsg: msg, err: err}
	}()

	close(startGate) // release both at the exact same moment
	wg.Wait()
	close(results)

	var claimedCount, rejectedCount int
	for r := range results {
		if r.err != nil {
			t.Fatalf("error reading from conn %s: %v", r.connID, r.err)
		}
		switch r.firstMsg["type"] {
		case "claimed":
			claimedCount++
		case "rejected":
			if r.firstMsg["reason"] != "bad code" {
				t.Fatalf("conn %s rejected with unexpected reason: %+v", r.connID, r.firstMsg)
			}
			rejectedCount++
		default:
			t.Fatalf("conn %s received unexpected message type: %+v", r.connID, r.firstMsg)
		}
	}

	if claimedCount != 1 || rejectedCount != 1 {
		t.Fatalf("expected exactly 1 claimed and 1 rejected 'bad code', got claimed=%d rejected=%d", claimedCount, rejectedCount)
	}
}
