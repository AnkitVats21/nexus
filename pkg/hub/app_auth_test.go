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

func setupTestHub(t *testing.T, allowSignup *bool) (*Hub, *httptest.Server, *http.Cookie, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nexus-app-auth-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "hub.db")
	jwtKeyPath := filepath.Join(dir, "jwt_ed25519.pem")

	h, err := New(Config{
		Version:      "test",
		Tokens:       []string{testToken},
		DBPath:       dbPath,
		TrustedProxy: "127.0.0.1",
		PublicURL:    "http://hub.local:8380",
		AllowSignup:  allowSignup,
		JWTKeyPath:   jwtKeyPath,
	})
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(h.Handler())

	// Run initial setup to create admin
	setupCode := h.Auth().SetupCode()
	validSetup := map[string]string{
		"setup_code":   setupCode,
		"username":     "admin",
		"display_name": "Admin User",
		"password":     "AdminPassword123!",
	}
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/setup", validSetup, nil)
	if res.StatusCode != http.StatusOK {
		srv.Close()
		if h.Store() != nil {
			h.Store().Close()
		}
		os.RemoveAll(dir)
		t.Fatalf("setup failed: %d, body: %s", res.StatusCode, string(body))
	}
	adminCookie := findCookie(res.Cookies(), auth.SessionCookieName)
	if adminCookie == nil {
		srv.Close()
		if h.Store() != nil {
			h.Store().Close()
		}
		os.RemoveAll(dir)
		t.Fatalf("missing session cookie")
	}

	cleanup := func() {
		srv.Close()
		if h.Store() != nil {
			h.Store().Close()
		}
		os.RemoveAll(dir)
	}
	return h, srv, adminCookie, cleanup
}

func doBearerReq(t *testing.T, srv *httptest.Server, method, path string, body any, token string, cookie *http.Cookie) (*http.Response, []byte) {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("do bearer request %s %s: %v", method, path, err)
	}
	resBody := new(bytes.Buffer)
	_, _ = resBody.ReadFrom(res.Body)
	res.Body.Close()
	return res, resBody.Bytes()
}

func TestAppAuth_Register_RateLimit_SignupOff_BeforeSetup(t *testing.T) {
	// 1. Before setup -> register returns 409
	dir, err := os.MkdirTemp("", "nexus-reg-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "hub.db")
	hBefore, err := New(Config{
		Version:      "test",
		Tokens:       []string{testToken},
		DBPath:       dbPath,
		TrustedProxy: "127.0.0.1",
		PublicURL:    "http://hub.local:8380",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srvBefore := httptest.NewServer(hBefore.Handler())
	defer srvBefore.Close()
	defer func() {
		if hBefore.Store() != nil {
			hBefore.Store().Close()
		}
	}()

	res, _ := doJSON(t, srvBefore, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "user1",
		"password": "Password123!",
	}, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 before setup, got %d", res.StatusCode)
	}

	// 2. AllowSignup = false -> register returns 403
	signupOff := false
	_, srvOff, _, cleanupOff := setupTestHub(t, &signupOff)
	defer cleanupOff()

	res, _ = doJSON(t, srvOff, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "user1",
		"password": "Password123!",
	}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when signup off, got %d", res.StatusCode)
	}

	// 3. Normal signup with rate limit (max 5 per hour)
	signupOn := true
	_, srvOn, _, cleanupOn := setupTestHub(t, &signupOn)
	defer cleanupOn()

	for i := 1; i <= 5; i++ {
		res, body := doJSON(t, srvOn, http.MethodPost, "/hub/api/register", map[string]string{
			"username":     fmt.Sprintf("user%d", i),
			"password":     "Password123!",
			"display_name": fmt.Sprintf("User %d", i),
		}, nil)
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("register %d failed: %d, body: %s", i, res.StatusCode, string(body))
		}
	}

	// 6th call -> 429
	res, _ = doJSON(t, srvOn, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "user6",
		"password": "Password123!",
	}, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 6th register, got %d", res.StatusCode)
	}
}

func TestAppAuth_AdminActions_Approve_Reject_Disable_Enable(t *testing.T) {
	ctx := context.Background()
	h, srv, adminCookie, cleanup := setupTestHub(t, nil)
	defer cleanup()

	// 1. Register pending user "charlie"
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "charlie",
		"password": "CharliePassword123!",
	}, nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("register charlie failed: %d", res.StatusCode)
	}
	charlie, err := h.Store().GetUserByUsername(ctx, "charlie")
	if err != nil || charlie.Status != "pending" {
		t.Fatalf("charlie should be pending, got %v, err: %v", charlie, err)
	}

	// 2. Reject pending user charlie -> deletes user
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/reject", charlie.ID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("reject charlie failed: %d", res.StatusCode)
	}
	_, err = h.Store().GetUserByID(ctx, charlie.ID)
	if err == nil {
		t.Fatalf("charlie should have been deleted")
	}

	// Rejecting active admin -> 400 Bad Request
	adminUser, _ := h.Store().GetUserByUsername(ctx, "admin")
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/reject", adminUser.ID), nil, adminCookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting active user, got %d", res.StatusCode)
	}

	// 3. Register pending user "david"
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "david",
		"password": "DavidPassword123!",
	}, nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("register david failed: %d", res.StatusCode)
	}
	david, err := h.Store().GetUserByUsername(ctx, "david")
	if err != nil {
		t.Fatalf("get david: %v", err)
	}

	// Approve david -> active
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/approve", david.ID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve david failed: %d", res.StatusCode)
	}
	david, _ = h.Store().GetUserByID(ctx, david.ID)
	if david.Status != "active" {
		t.Fatalf("david should be active, got %s", david.Status)
	}

	// David logs in and gets cookie + token
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "david",
		"password": "DavidPassword123!",
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("david login failed: %d, body: %s", res.StatusCode, string(body))
	}
	davidCookie := findCookie(res.Cookies(), auth.SessionCookieName)

	// David gets a pairing code and pairs an edge device
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/pairing", nil, davidCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("david pairing code failed: %d", res.StatusCode)
	}
	var pData map[string]any
	_ = json.Unmarshal(body, &pData)
	pairCode := pData["code"].(string)

	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http") + EdgePath
	connEdge, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connEdge.Close(websocket.StatusNormalClosure, "")
	write(t, ctx, connEdge, map[string]any{
		"type":      "hello",
		"proto":     3,
		"id":        "nexus-david-edge",
		"name":      "David Edge",
		"kind":      "esp32-s3",
		"pair_code": pairCode,
	})
	claimedMsg := read(t, ctx, connEdge)
	edgeToken := claimedMsg["token"].(string)
	_ = read(t, ctx, connEdge) // welcome

	// David also requests JWT token & refresh token
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": "david",
		"password": "DavidPassword123!",
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth token failed: %d", res.StatusCode)
	}
	var tokenResp map[string]any
	_ = json.Unmarshal(body, &tokenResp)
	davidBearer := tokenResp["access_token"].(string)
	davidRefresh := tokenResp["refresh_token"].(string)

	// 4. Disable active user david
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/disable", david.ID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable david failed: %d", res.StatusCode)
	}

	david, _ = h.Store().GetUserByID(ctx, david.ID)
	if david.Status != "disabled" {
		t.Fatalf("david status should be disabled, got %s", david.Status)
	}

	// Verify david's edge connection was closed immediately
	edgeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, readErr := connEdge.Read(edgeCtx)
	if readErr == nil {
		t.Fatalf("expected edge read error after user disabled")
	}

	// Edge tries to reconnect with its existing token
	connEdge2, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer connEdge2.Close(websocket.StatusNormalClosure, "")
	write(t, ctx, connEdge2, map[string]any{
		"type":  "hello",
		"proto": 3,
		"id":    "nexus-david-edge",
		"token": edgeToken,
	})
	rejectMsg := read(t, ctx, connEdge2)
	if rejectMsg["type"] != "rejected" || rejectMsg["reason"] != "disabled" {
		t.Fatalf("expected rejected disabled, got %v", rejectMsg)
	}

	// Device record in DB should NOT be deleted
	dev, err := h.Store().GetDeviceByID(ctx, "nexus-david-edge")
	if err != nil || dev == nil {
		t.Fatalf("device record should still exist in DB")
	}

	// Refresh token cannot be refreshed
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/auth/refresh", map[string]string{
		"refresh_token": davidRefresh,
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 refresh for disabled user, got %d", res.StatusCode)
	}

	// Bearer token cannot access /hub/api/me
	res, _ = doBearerReq(t, srv, http.MethodGet, "/hub/api/me", nil, davidBearer, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 bearer for disabled user, got %d", res.StatusCode)
	}

	// 5. Enable david
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/enable", david.ID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("enable david failed: %d", res.StatusCode)
	}

	// Edge can now reconnect
	connEdge3, _, err := websocket.Dial(ctx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial 3: %v", err)
	}
	defer connEdge3.Close(websocket.StatusNormalClosure, "")
	write(t, ctx, connEdge3, map[string]any{
		"type":  "hello",
		"proto": 3,
		"id":    "nexus-david-edge",
		"token": edgeToken,
	})
	welcomeMsg := read(t, ctx, connEdge3)
	if welcomeMsg["type"] != "welcome" {
		t.Fatalf("expected welcome after re-enable, got %v", welcomeMsg)
	}

	// 6. Disable last active admin -> rejected
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/disable", adminUser.ID), nil, adminCookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when disabling last admin, got %d", res.StatusCode)
	}
}

func TestAppAuth_LoginGating(t *testing.T) {
	_, srv, adminCookie, cleanup := setupTestHub(t, nil)
	defer cleanup()

	// Register "eve" (pending)
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "eve",
		"password": "EvePassword123!",
	}, nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("register eve failed: %d", res.StatusCode)
	}

	// Wrong password -> 401 (does NOT leak status)
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "eve",
		"password": "WrongPassword999!",
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong password, got %d", res.StatusCode)
	}

	// Correct password -> 403 pending_approval
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "eve",
		"password": "EvePassword123!",
	}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on pending user, got %d", res.StatusCode)
	}
	var errData map[string]any
	_ = json.Unmarshal(body, &errData)
	if errData["code"] != "pending_approval" {
		t.Fatalf("expected code pending_approval, got %v", errData)
	}

	// Token endpoint also returns 403 pending_approval
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": "eve",
		"password": "EvePassword123!",
	}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 from token endpoint, got %d", res.StatusCode)
	}
	_ = json.Unmarshal(body, &errData)
	if errData["code"] != "pending_approval" {
		t.Fatalf("expected code pending_approval, got %v", errData)
	}

	// Approve eve
	var usersList []map[string]any
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/users?status=pending", nil, adminCookie)
	_ = json.Unmarshal(body, &usersList)
	var eveID int64
	for _, u := range usersList {
		if u["username"] == "eve" {
			eveID = int64(u["id"].(float64))
			break
		}
	}
	if eveID == 0 {
		t.Fatalf("eve not found in pending list")
	}

	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/approve", eveID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve eve failed: %d", res.StatusCode)
	}

	// Disable eve
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/disable", eveID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("disable eve failed: %d", res.StatusCode)
	}

	// Login with wrong password -> 401
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "eve",
		"password": "WrongPassword999!",
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on wrong password for disabled user, got %d", res.StatusCode)
	}

	// Login with correct password -> 403 disabled
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/login", map[string]string{
		"username": "eve",
		"password": "EvePassword123!",
	}, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 on disabled user, got %d", res.StatusCode)
	}
	_ = json.Unmarshal(body, &errData)
	if errData["code"] != "disabled" {
		t.Fatalf("expected code disabled, got %v", errData)
	}
}

func TestAppAuth_Token_Refresh_Reuse_Logout(t *testing.T) {
	_, srv, adminCookie, cleanup := setupTestHub(t, nil)
	defer cleanup()

	// Register & approve user "frank"
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "frank",
		"password": "FrankPassword123!",
	}, nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("register frank failed: %d", res.StatusCode)
	}

	res, body := doJSON(t, srv, http.MethodGet, "/hub/api/users?status=pending", nil, adminCookie)
	var usersList []map[string]any
	_ = json.Unmarshal(body, &usersList)
	var frankID int64
	for _, u := range usersList {
		if u["username"] == "frank" {
			frankID = int64(u["id"].(float64))
			break
		}
	}
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/approve", frankID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve frank failed: %d", res.StatusCode)
	}

	// 1. POST /hub/api/auth/token -> access_token + refresh_token
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": "frank",
		"password": "FrankPassword123!",
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth token failed: %d", res.StatusCode)
	}
	var tokenData map[string]any
	_ = json.Unmarshal(body, &tokenData)
	access1 := tokenData["access_token"].(string)
	refresh1 := tokenData["refresh_token"].(string)
	if access1 == "" || refresh1 == "" {
		t.Fatalf("expected non-empty tokens")
	}

	// Verify JWKS endpoint
	res, body = doJSON(t, srv, http.MethodGet, "/hub/api/auth/jwks", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /hub/api/auth/jwks failed: %d", res.StatusCode)
	}
	var jwks map[string]any
	_ = json.Unmarshal(body, &jwks)
	keys := jwks["keys"].([]any)
	if len(keys) == 0 {
		t.Fatalf("expected keys in JWKS")
	}

	// 2. Refresh: POST /hub/api/auth/refresh with refresh1 -> access2 + refresh2
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/auth/refresh", map[string]string{
		"refresh_token": refresh1,
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth refresh failed: %d, body: %s", res.StatusCode, string(body))
	}
	_ = json.Unmarshal(body, &tokenData)
	refresh2 := tokenData["refresh_token"].(string)
	if refresh2 == "" || refresh2 == refresh1 {
		t.Fatalf("expected new refresh token")
	}

	// 3. Reuse refresh1 -> 401 AND invalidates refresh2 (family deletion)
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/auth/refresh", map[string]string{
		"refresh_token": refresh1,
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 reusing refresh1, got %d", res.StatusCode)
	}

	// Calling refresh with refresh2 should now ALSO fail because family was revoked
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/auth/refresh", map[string]string{
		"refresh_token": refresh2,
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on refresh2 after family revocation, got %d", res.StatusCode)
	}

	// 4. Logout: get a fresh token family, then call /auth/logout
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": "frank",
		"password": "FrankPassword123!",
	}, nil)
	_ = json.Unmarshal(body, &tokenData)
	refresh3 := tokenData["refresh_token"].(string)

	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/auth/logout", map[string]string{
		"refresh_token": refresh3,
	}, nil)
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout failed: %d", res.StatusCode)
	}

	// refresh3 should now fail
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/auth/refresh", map[string]string{
		"refresh_token": refresh3,
	}, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", res.StatusCode)
	}
}

func TestAppAuth_DeviceRegister(t *testing.T) {
	ctx := context.Background()
	h, srv, adminCookie, cleanup := setupTestHub(t, nil)
	defer cleanup()

	// Setup user "grace"
	res, _ := doJSON(t, srv, http.MethodPost, "/hub/api/register", map[string]string{
		"username": "grace",
		"password": "GracePassword123!",
	}, nil)
	res, body := doJSON(t, srv, http.MethodGet, "/hub/api/users?status=pending", nil, adminCookie)
	var usersList []map[string]any
	_ = json.Unmarshal(body, &usersList)
	var graceID int64
	for _, u := range usersList {
		if u["username"] == "grace" {
			graceID = int64(u["id"].(float64))
			break
		}
	}
	res, _ = doJSON(t, srv, http.MethodPost, fmt.Sprintf("/hub/api/users/%d/approve", graceID), nil, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("approve grace failed: %d", res.StatusCode)
	}

	// Grace gets Bearer token
	res, body = doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": "grace",
		"password": "GracePassword123!",
	}, nil)
	var tokenData map[string]any
	_ = json.Unmarshal(body, &tokenData)
	graceBearer := tokenData["access_token"].(string)

	// 1. Non-android kind -> 400
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/devices/register", map[string]string{
		"id":   "nexus-app-phone1",
		"name": "Grace Phone",
		"kind": "board",
	}, graceBearer, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-android kind, got %d", res.StatusCode)
	}

	// 2. Invalid ID format -> 400
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/devices/register", map[string]string{
		"id":   "bad*id!",
		"name": "Grace Phone",
		"kind": "android",
	}, graceBearer, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid id, got %d", res.StatusCode)
	}

	// 3. New device -> 200 OK + device_token
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/devices/register", map[string]string{
		"id":   "nexus-app-phone1",
		"name": "Grace Phone",
		"kind": "android",
	}, graceBearer, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("device register failed: %d, body: %s", res.StatusCode, string(body))
	}
	var devResp map[string]string
	_ = json.Unmarshal(body, &devResp)
	tok1 := devResp["device_token"]
	if tok1 == "" {
		t.Fatalf("expected device_token in response")
	}

	// Check DB
	dev, err := h.Store().GetDeviceByID(ctx, "nexus-app-phone1")
	if err != nil || dev.Kind != "android" || dev.OwnerID != graceID {
		t.Fatalf("invalid device in DB: %v, err: %v", dev, err)
	}

	// 4. Re-registering existing device owned by same user -> rotates token
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/devices/register", map[string]string{
		"id":   "nexus-app-phone1",
		"name": "Grace Phone Renamed",
		"kind": "android",
	}, graceBearer, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("device re-register failed: %d", res.StatusCode)
	}
	_ = json.Unmarshal(body, &devResp)
	tok2 := devResp["device_token"]
	if tok2 == "" || tok2 == tok1 {
		t.Fatalf("expected rotated device_token")
	}

	dev, _ = h.Store().GetDeviceByID(ctx, "nexus-app-phone1")
	if dev.Name != "Grace Phone Renamed" {
		t.Fatalf("expected name updated to Grace Phone Renamed, got %s", dev.Name)
	}

	// 5. Another user (admin) registering device with same ID -> 409 Conflict
	res, _ = doJSON(t, srv, http.MethodPost, "/hub/api/devices/register", map[string]string{
		"id":   "nexus-app-phone1",
		"name": "Admin Steals Phone",
		"kind": "android",
	}, adminCookie)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 conflict registering existing device from another user, got %d", res.StatusCode)
	}

	// 6. Empty ID -> generated by hub
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/devices/register", map[string]string{
		"name": "Grace Tablet",
		"kind": "android",
	}, graceBearer, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("register with empty id failed: %d, body: %s", res.StatusCode, string(body))
	}
	_ = json.Unmarshal(body, &devResp)
	if devResp["device_token"] == "" {
		t.Fatalf("expected device_token on auto-generated id")
	}
}

func TestAppAuth_BearerAuth_And_CSRFRules(t *testing.T) {
	_, srv, adminCookie, cleanup := setupTestHub(t, nil)
	defer cleanup()

	// Admin requests Bearer token
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": "admin",
		"password": "AdminPassword123!",
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth token failed: %d", res.StatusCode)
	}
	var tokenData map[string]any
	_ = json.Unmarshal(body, &tokenData)
	adminBearer := tokenData["access_token"].(string)

	// 1. Bearer auth on /hub/api/me with NO CSRF (no Origin) -> 200 OK
	res, body = doBearerReq(t, srv, http.MethodGet, "/hub/api/me", nil, adminBearer, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bearer /me without CSRF, got %d, body: %s", res.StatusCode, string(body))
	}

	// 2. Cookie session request to state-changing endpoint without CSRF -> 403 Forbidden
	// Notice doBearerReq does not set Origin header!
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/pairing", nil, "", adminCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 CSRF forbidden for cookie session without Origin header, got %d", res.StatusCode)
	}

	// 3. Request with BOTH cookie and Bearer without Origin -> still checked for CSRF -> 403 Forbidden
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/pairing", nil, adminBearer, adminCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 CSRF forbidden when cookie is present even with Bearer, got %d", res.StatusCode)
	}

	// 4. Request with Bearer ONLY to state-changing endpoint without Origin -> 200 OK
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/pairing", nil, adminBearer, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for bearer pairing POST without CSRF, got %d", res.StatusCode)
	}
}
