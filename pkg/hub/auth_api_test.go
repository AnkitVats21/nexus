package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"nexus/pkg/auth"
	"os"
	"path/filepath"
	"strings"
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
