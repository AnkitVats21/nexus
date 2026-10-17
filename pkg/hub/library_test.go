package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nexus/pkg/store"
)

func getAuthToken(t *testing.T, srv *httptest.Server, username, password string) string {
	t.Helper()
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/auth/token", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth token failed: status %d, body: %s", res.StatusCode, string(body))
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.AccessToken == "" {
		t.Fatalf("failed to decode access_token: %v, body: %s", err, string(body))
	}
	return resp.AccessToken
}

func createUser(t *testing.T, srv *httptest.Server, adminCookie *http.Cookie, username, password string) {
	t.Helper()
	res, body := doJSON(t, srv, http.MethodPost, "/hub/api/users", map[string]string{
		"username":     username,
		"display_name": username,
		"password":     password,
		"role":         "member",
	}, adminCookie)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create user %s failed: status %d, body: %s", username, res.StatusCode, string(body))
	}
}

func sampleTrack(id, title string) Track {
	return Track{
		ID:        id,
		Title:     title,
		Artist:    "Test Artist",
		Album:     "Test Album",
		Duration:  215,
		Thumbnail: "https://example.com/thumb.jpg",
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 1: Auth & CSRF rules
// Bearer JWT without CSRF -> allowed
// Session cookie without CSRF -> 403 Forbidden
// Unauthenticated -> 401 Unauthorized
// MustChange password -> 403 Forbidden
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_AuthAndCSRFRules(t *testing.T) {
	h, srv, adminCookie, cleanup := setupTestHub(t, nil)
	defer cleanup()

	// 1. Unauthenticated -> 401
	res, _ := doBearerReq(t, srv, http.MethodGet, "/hub/api/me/likes", nil, "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", res.StatusCode)
	}

	// 2. Cookie without CSRF for write -> 403
	tr := sampleTrack("test_track1", "Test Song")
	res, _ = doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/test_track1", map[string]any{"track": tr}, "", adminCookie)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cookie without CSRF, got %d", res.StatusCode)
	}

	// 3. Bearer JWT without CSRF -> 200 OK
	adminToken := getAuthToken(t, srv, "admin", "AdminPassword123!")
	res, body := doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/test_track1", map[string]any{"track": tr}, adminToken, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 bearer write without CSRF, got %d: %s", res.StatusCode, string(body))
	}

	// 4. Must change user -> 403
	ctx := context.Background()
	_ = h.Store().CreateUser(ctx, &store.User{
		Username:    "mustchange1",
		DisplayName: "Must Change",
		Role:        "member",
		PassHash:    "dummy",
		MustChange:  true,
		Status:      "active",
	})
	uMC, _ := h.Store().GetUserByUsername(ctx, "mustchange1")
	mcToken, _, _ := h.jwt.SignUserToken(uMC)

	res, _ = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/likes", nil, mcToken, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for must_change user, got %d", res.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 2: Track Validation (400 invalid_track)
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_TrackValidation(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	cases := []struct {
		name  string
		track Track
	}{
		{"short id", Track{ID: "too_short", Title: "Valid", Duration: 100}},
		{"long id", Track{ID: "too_long_id_123", Title: "Valid", Duration: 100}},
		{"special char id", Track{ID: "bad@char!!!", Title: "Valid", Duration: 100}},
		{"empty title", Track{ID: "vid_11char1", Title: "   ", Duration: 100}},
		{"title too long", Track{ID: "vid_11char1", Title: strings.Repeat("A", 301), Duration: 100}},
		{"artist too long", Track{ID: "vid_11char1", Title: "Valid", Artist: strings.Repeat("B", 301), Duration: 100}},
		{"album too long", Track{ID: "vid_11char1", Title: "Valid", Album: strings.Repeat("C", 301), Duration: 100}},
		{"negative duration", Track{ID: "vid_11char1", Title: "Valid", Duration: -1}},
		{"duration over 24h", Track{ID: "vid_11char1", Title: "Valid", Duration: 86401}},
		{"insecure http thumb", Track{ID: "vid_11char1", Title: "Valid", Duration: 100, Thumbnail: "http://example.com/t.jpg"}},
		{"thumbnail too long", Track{ID: "vid_11char1", Title: "Valid", Duration: 100, Thumbnail: "https://example.com/" + strings.Repeat("x", 501)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/"+tc.track.ID, map[string]any{"track": tc.track}, token, nil)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s, got %d: %s", tc.name, res.StatusCode, string(body))
			}
			var errResp struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(body, &errResp)
			if errResp.Error.Code != "invalid_track" {
				t.Fatalf("expected code invalid_track, got %s", errResp.Error.Code)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 3: Likes API (CRUD, Compact IDs, Idempotency)
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_LikesAPI(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	t1 := sampleTrack("vid_like001", "Song One")
	t2 := sampleTrack("vid_like002", "Song Two")

	// 1. PUT likes
	res, _ := doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/vid_like001", map[string]any{"track": t1}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("put like 1 failed: %d", res.StatusCode)
	}

	res, _ = doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/vid_like002", map[string]any{"track": t2}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("put like 2 failed: %d", res.StatusCode)
	}

	// 2. GET /me/likes
	res, body := doBearerReq(t, srv, http.MethodGet, "/hub/api/me/likes", nil, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get likes failed: %d", res.StatusCode)
	}
	var likesResp struct {
		Items []struct {
			LikedAt int64 `json:"liked_at"`
			Track   Track `json:"track"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &likesResp)
	if len(likesResp.Items) != 2 {
		t.Fatalf("expected 2 liked items, got %d", len(likesResp.Items))
	}
	if likesResp.Items[0].Track.ID != "vid_like002" {
		t.Fatalf("expected newest like first, got %s", likesResp.Items[0].Track.ID)
	}

	// 3. Compact IDs GET /me/likes/ids
	res, body = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/likes/ids", nil, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get likes ids failed: %d", res.StatusCode)
	}
	var idsResp struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(body, &idsResp)
	if len(idsResp.IDs) != 2 || idsResp.IDs[0] != "vid_like002" || idsResp.IDs[1] != "vid_like001" {
		t.Fatalf("unexpected ids: %v", idsResp.IDs)
	}

	// 4. DELETE /me/likes/{id} -> 204
	res, _ = doBearerReq(t, srv, http.MethodDelete, "/hub/api/me/likes/vid_like001", nil, token, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 on delete like, got %d", res.StatusCode)
	}

	// Idempotent delete -> still 204
	res, _ = doBearerReq(t, srv, http.MethodDelete, "/hub/api/me/likes/vid_like001", nil, token, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 on repeated delete like, got %d", res.StatusCode)
	}

	// Check remaining
	res, body = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/likes/ids", nil, token, nil)
	_ = json.Unmarshal(body, &idsResp)
	if len(idsResp.IDs) != 1 || idsResp.IDs[0] != "vid_like002" {
		t.Fatalf("expected only vid_like002, got %v", idsResp.IDs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 4: Playlists API, Ownership & 404 Across Users
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_PlaylistsAPI_AndOwnership(t *testing.T) {
	h, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token1 := getAuthToken(t, srv, "admin", "AdminPassword123!")

	ctx := context.Background()
	_ = h.Store().CreateUser(ctx, &store.User{
		Username:    "user2",
		DisplayName: "User Two",
		Role:        "member",
		PassHash:    "dummy",
		Status:      "active",
	})
	u2, err := h.Store().GetUserByUsername(ctx, "user2")
	if err != nil {
		t.Fatalf("get user2: %v", err)
	}
	token2, _, err := h.jwt.SignUserToken(u2)
	if err != nil {
		t.Fatalf("sign token2: %v", err)
	}

	// 1. User 1 creates playlist
	res, body := doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists", map[string]string{"name": "User 1 Mix"}, token1, nil)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 created playlist, got %d: %s", res.StatusCode, string(body))
	}
	var pl struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version int64  `json:"version"`
	}
	_ = json.Unmarshal(body, &pl)
	if pl.ID == "" || pl.Name != "User 1 Mix" || pl.Version != 1 {
		t.Fatalf("unexpected playlist: %+v", pl)
	}

	// 2. User 2 tries to GET user 1's playlist -> MUST BE 404 (NEVER 403)
	res, _ = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/playlists/"+pl.ID, nil, token2, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 accessing foreign playlist, got %d", res.StatusCode)
	}

	// 3. User 2 tries to PATCH user 1's playlist -> 404
	res, _ = doBearerReq(t, srv, http.MethodPatch, "/hub/api/me/playlists/"+pl.ID, map[string]string{"name": "Hacked"}, token2, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 patching foreign playlist, got %d", res.StatusCode)
	}

	// 4. User 2 tries to DELETE user 1's playlist -> 404
	res, _ = doBearerReq(t, srv, http.MethodDelete, "/hub/api/me/playlists/"+pl.ID, nil, token2, nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 deleting foreign playlist, got %d", res.StatusCode)
	}

	// 5. User 1 patches playlist -> 200, returns version 2
	res, body = doBearerReq(t, srv, http.MethodPatch, "/hub/api/me/playlists/"+pl.ID, map[string]any{
		"name":    "User 1 Chill",
		"version": 1,
	}, token1, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 patch, got %d: %s", res.StatusCode, string(body))
	}
	var patchResp struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(body, &patchResp)
	if patchResp.Version != 2 {
		t.Fatalf("expected version 2, got %d", patchResp.Version)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 5: Playlist Items: Dense Positions, Move, Delete, Thumbnails
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_PlaylistItems_DenseAndReorder(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	// Create playlist
	res, body := doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists", map[string]string{"name": "Reorder Test"}, token, nil)
	var pl struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &pl)

	t1 := sampleTrack("vid_dense01", "Song 1")
	t2 := sampleTrack("vid_dense02", "Song 2")
	t3 := sampleTrack("vid_dense03", "Song 3")
	t4 := sampleTrack("vid_dense04", "Song 4")

	// 1. Add 3 tracks
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists/"+pl.ID+"/items", map[string]any{
		"tracks":  []Track{t1, t2, t3},
		"version": 1,
	}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("add items failed: %d, body: %s", res.StatusCode, string(body))
	}
	var addResp struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(body, &addResp)
	if addResp.Version != 2 {
		t.Fatalf("expected version 2 after add, got %d", addResp.Version)
	}

	// 2. Insert t4 at position 1
	pos := 1
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists/"+pl.ID+"/items", map[string]any{
		"tracks":   []Track{t4},
		"position": pos,
		"version":  2,
	}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("insert at pos 1 failed: %d: %s", res.StatusCode, string(body))
	}

	// Verify order: t1 (0), t4 (1), t2 (2), t3 (3)
	res, body = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/playlists/"+pl.ID, nil, token, nil)
	var getResp struct {
		Version int64 `json:"version"`
		Items   []struct {
			ItemID   string `json:"item_id"`
			Position int    `json:"position"`
			Track    Track  `json:"track"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &getResp)
	if len(getResp.Items) != 4 {
		t.Fatalf("expected 4 items, got %d", len(getResp.Items))
	}
	expectedOrder := []string{"vid_dense01", "vid_dense04", "vid_dense02", "vid_dense03"}
	for i, expectedID := range expectedOrder {
		if getResp.Items[i].Track.ID != expectedID {
			t.Fatalf("item %d expected %s, got %s", i, expectedID, getResp.Items[i].Track.ID)
		}
		if getResp.Items[i].Position != i {
			t.Fatalf("item %d position not dense: %d", i, getResp.Items[i].Position)
		}
	}

	// 3. Move t3 (index 3) to position 0
	t3ItemID := getResp.Items[3].ItemID
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists/"+pl.ID+"/move", map[string]any{
		"item_id": t3ItemID,
		"to":      0,
		"version": 3,
	}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("move failed: %d, body: %s", res.StatusCode, string(body))
	}

	// Order should now be: t3 (0), t1 (1), t4 (2), t2 (3)
	res, body = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/playlists/"+pl.ID, nil, token, nil)
	_ = json.Unmarshal(body, &getResp)
	expectedOrderAfterMove := []string{"vid_dense03", "vid_dense01", "vid_dense04", "vid_dense02"}
	for i, expectedID := range expectedOrderAfterMove {
		if getResp.Items[i].Track.ID != expectedID {
			t.Fatalf("after move: item %d expected %s, got %s", i, expectedID, getResp.Items[i].Track.ID)
		}
		if getResp.Items[i].Position != i {
			t.Fatalf("after move: position not dense: %d", getResp.Items[i].Position)
		}
	}

	// 4. Delete item t4 (index 2)
	t4ItemID := getResp.Items[2].ItemID
	res, body = doBearerReq(t, srv, http.MethodDelete, "/hub/api/me/playlists/"+pl.ID+"/items/"+t4ItemID+"?version=4", nil, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete item failed: %d: %s", res.StatusCode, string(body))
	}

	// Final order: t3 (0), t1 (1), t2 (2)
	res, body = doBearerReq(t, srv, http.MethodGet, "/hub/api/me/playlists/"+pl.ID, nil, token, nil)
	_ = json.Unmarshal(body, &getResp)
	if len(getResp.Items) != 3 {
		t.Fatalf("expected 3 items after delete, got %d", len(getResp.Items))
	}
	expectedOrderAfterDel := []string{"vid_dense03", "vid_dense01", "vid_dense02"}
	for i, expectedID := range expectedOrderAfterDel {
		if getResp.Items[i].Track.ID != expectedID {
			t.Fatalf("after del: item %d expected %s, got %s", i, expectedID, getResp.Items[i].Track.ID)
		}
		if getResp.Items[i].Position != i {
			t.Fatalf("after del: position not dense: %d", getResp.Items[i].Position)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 6: Version Conflicts (409) and Limits (422)
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_VersionConflictAndLimits(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	// 1. Limit: Playlist name > 100 characters -> 422 limit
	longName := strings.Repeat("A", 101)
	res, body := doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists", map[string]string{"name": longName}, token, nil)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for name > 100, got %d: %s", res.StatusCode, string(body))
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &errResp)
	if errResp.Error.Code != "limit" {
		t.Fatalf("expected code limit, got %s", errResp.Error.Code)
	}

	// Create valid playlist
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists", map[string]string{"name": "Conflict Playlist"}, token, nil)
	var pl struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	_ = json.Unmarshal(body, &pl)

	// 2. Version conflict on PATCH
	res, body = doBearerReq(t, srv, http.MethodPatch, "/hub/api/me/playlists/"+pl.ID, map[string]any{
		"name":    "Updated",
		"version": 999,
	}, token, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 version conflict on patch, got %d: %s", res.StatusCode, string(body))
	}
	var conflictResp struct {
		CurrentVersion int64 `json:"current_version"`
	}
	_ = json.Unmarshal(body, &conflictResp)
	if conflictResp.CurrentVersion != 1 {
		t.Fatalf("expected current_version 1, got %d", conflictResp.CurrentVersion)
	}

	// 3. Version conflict on Add items
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/playlists/"+pl.ID+"/items", map[string]any{
		"tracks":  []Track{sampleTrack("vid_conf001", "Song 1")},
		"version": 999,
	}, token, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on add items with bad version, got %d: %s", res.StatusCode, string(body))
	}

	// 4. History limit: > 200 entries in one call -> 422 limit
	oversized := make([]map[string]any, 201)
	for i := 0; i < 201; i++ {
		oversized[i] = map[string]any{
			"track":     sampleTrack(fmt.Sprintf("vid_h%06d", i), "Song"),
			"played_at": time.Now().UTC().UnixMilli(),
			"played_ms": 1000,
		}
	}
	res, body = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/history", map[string]any{"entries": oversized}, token, nil)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for > 200 history entries, got %d: %s", res.StatusCode, string(body))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 7: History API: Time-window rules, device ownership, dedupe, paging
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_HistoryAPI_AndPaging(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	nowMs := time.Now().UTC().UnixMilli()
	futureMs := nowMs + 6*60*1000      // +6 minutes (beyond 5 min window)
	pastMs := nowMs - 31*24*60*60*1000 // -31 days (beyond 30 days window)
	validMs := nowMs - 1000

	// 1. Future timestamp > 5m -> 400
	res, _ := doBearerReq(t, srv, http.MethodPost, "/hub/api/me/history", map[string]any{
		"entries": []map[string]any{
			{"track": sampleTrack("vid_hist001", "Future"), "played_at": futureMs, "played_ms": 1000},
		},
	}, token, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for future timestamp > 5 min, got %d", res.StatusCode)
	}

	// 2. Past timestamp > 30d -> 400
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/history", map[string]any{
		"entries": []map[string]any{
			{"track": sampleTrack("vid_hist001", "Ancient"), "played_at": pastMs, "played_ms": 1000},
		},
	}, token, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for timestamp > 30 days old, got %d", res.StatusCode)
	}

	// 3. Valid batch with 1 duplicate -> accepted count = 2
	entries := []map[string]any{
		{"track": sampleTrack("vid_hist001", "Song 1"), "played_at": validMs, "played_ms": 100000},
		{"track": sampleTrack("vid_hist002", "Song 2"), "played_at": validMs + 10, "played_ms": 120000},
		{"track": sampleTrack("vid_hist001", "Song 1"), "played_at": validMs, "played_ms": 100000}, // duplicate!
	}
	res, body := doBearerReq(t, srv, http.MethodPost, "/hub/api/me/history", map[string]any{"entries": entries}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("post history failed: %d, body: %s", res.StatusCode, string(body))
	}
	var histResp struct {
		Accepted int `json:"accepted"`
	}
	_ = json.Unmarshal(body, &histResp)
	if histResp.Accepted != 2 {
		t.Fatalf("expected accepted 2, got %d", histResp.Accepted)
	}

	// 4. Cursor paging with no gaps or repeats
	// Add 25 more distinct history rows
	moreEntries := make([]map[string]any, 25)
	for i := 0; i < 25; i++ {
		moreEntries[i] = map[string]any{
			"track":     sampleTrack(fmt.Sprintf("vid_pag%04d", i), fmt.Sprintf("Song %d", i)),
			"played_at": validMs - int64((i+1)*1000),
			"played_ms": 50000,
		}
	}
	res, _ = doBearerReq(t, srv, http.MethodPost, "/hub/api/me/history", map[string]any{"entries": moreEntries}, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("post more history failed: %d", res.StatusCode)
	}

	// Total rows = 2 + 25 = 27
	// Fetch with limit = 10 across pages
	seenIDs := make(map[int64]bool)
	cursor := ""
	pageCount := 0
	totalFetched := 0

	for {
		url := "/hub/api/me/history?limit=10"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		res, body = doBearerReq(t, srv, http.MethodGet, url, nil, token, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("get history page failed: %d", res.StatusCode)
		}
		var pageResp struct {
			Items []struct {
				ID int64 `json:"id"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		_ = json.Unmarshal(body, &pageResp)
		pageCount++
		totalFetched += len(pageResp.Items)

		for _, it := range pageResp.Items {
			if seenIDs[it.ID] {
				t.Fatalf("duplicate history item id %d across pages", it.ID)
			}
			seenIDs[it.ID] = true
		}

		if pageResp.NextCursor == "" {
			break
		}
		cursor = pageResp.NextCursor
	}

	if totalFetched != 27 {
		t.Fatalf("expected 27 total fetched across pages, got %d", totalFetched)
	}
	if pageCount != 3 { // 10, 10, 7
		t.Fatalf("expected 3 pages, got %d", pageCount)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 8: ETags and 304 Not Modified
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_ETagAndSyncVersions(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	// 1. GET /me/library/versions -> initial 0
	res, body := doBearerReq(t, srv, http.MethodGet, "/hub/api/me/library/versions", nil, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get library versions failed: %d", res.StatusCode)
	}
	var vers struct {
		Likes     int64 `json:"likes"`
		Playlists int64 `json:"playlists"`
		History   int64 `json:"history"`
	}
	_ = json.Unmarshal(body, &vers)
	if vers.Likes != 0 || vers.Playlists != 0 || vers.History != 0 {
		t.Fatalf("expected all 0 versions, got %+v", vers)
	}

	// 2. Put like -> bumps likes_version
	_ = sampleTrack("vid_etag001", "ETag Song")
	_, _ = doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/vid_etag001", map[string]any{
		"track": sampleTrack("vid_etag001", "ETag Song"),
	}, token, nil)

	// Fetch likes with ETag
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/hub/api/me/likes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, _ := client.Do(req)
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" {
		t.Fatalf("missing ETag header on likes response")
	}

	// Repeat with If-None-Match -> MUST answer 304 Not Modified
	req304, _ := http.NewRequest(http.MethodGet, srv.URL+"/hub/api/me/likes", nil)
	req304.Header.Set("Authorization", "Bearer "+token)
	req304.Header.Set("If-None-Match", etag)
	resp304, _ := client.Do(req304)
	resp304.Body.Close()
	if resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", resp304.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 9: Hub-Played History Hook
// Simulates board playback with owner ID and verifies history recorded with source 'board'
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_HubPlayedHistoryHook(t *testing.T) {
	h, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	// 1. Get admin user id
	ctx := context.Background()
	admin, err := h.Store().GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("get admin user: %v", err)
	}

	// 2. Create board edge and register it as an active player
	boardEdge := &edge{
		id:      "board-speaker-01",
		name:    "Living Room Speaker",
		kind:    "esp32",
		roles:   edgeRoles{Player: &playerRole{URL: true}},
		ownerID: admin.ID,
		send:    make(chan []byte, 16),
	}
	_ = h.Store().UpsertDevice(ctx, &store.Device{
		ID:        boardEdge.id,
		OwnerID:   admin.ID,
		Name:      boardEdge.name,
		Kind:      boardEdge.kind,
		TokenHash: [32]byte{1, 2, 3},
		CreatedAt: time.Now().Unix(),
	})

	h.mu.Lock()
	h.edges[boardEdge.id] = boardEdge
	h.mu.Unlock()

	// Initialize music controller with fakeTracks if nil
	if h.music == nil {
		h.music = newMusic(h, &fakeTracks{})
	}

	// Set music controller output to board edge
	h.music.mu.Lock()
	h.music.output = boardEdge.id
	song := Track{
		ID:       "vid_board01",
		Title:    "Board Song",
		Artist:   "Speaker Artist",
		Duration: 180,
	}
	h.music.startLocked(song, "http://example.com/audio.opus", 0, false)
	curSeq := h.music.seq
	h.music.mu.Unlock()

	// Drain send channel
	select {
	case <-boardEdge.send:
	default:
	}

	// 3. Simulate report at 10s (< 30s) -> should NOT record history yet
	h.music.report(boardEdge, musicReport{
		Seq:        curSeq,
		ID:         song.ID,
		State:      "playing",
		PositionMs: 10000,
		DurationMs: 180000,
	})
	time.Sleep(50 * time.Millisecond)

	hist, err := h.Store().GetHistory(ctx, admin.ID, 10, 0)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("history should not be recorded at 10s, got %d", len(hist))
	}

	// 4. Simulate report at 30s (>= 30000) -> MUST record history with source 'board'
	h.music.report(boardEdge, musicReport{
		Seq:        curSeq,
		ID:         song.ID,
		State:      "playing",
		PositionMs: 30500,
		DurationMs: 180000,
	})
	time.Sleep(100 * time.Millisecond)

	hist, err = h.Store().GetHistory(ctx, admin.ID, 10, 0)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("expected 1 history entry after 30s, got %d", len(hist))
	}
	if hist[0].Source != "board" {
		t.Fatalf("expected source 'board', got %s", hist[0].Source)
	}
	if hist[0].VideoID != song.ID {
		t.Fatalf("expected video_id %s, got %s", song.ID, hist[0].VideoID)
	}
	if hist[0].DeviceID == nil || *hist[0].DeviceID != boardEdge.id {
		t.Fatalf("expected device_id %s, got %v", boardEdge.id, hist[0].DeviceID)
	}

	// 5. Test track ending before 30s (e.g. short 20s song)
	shortSong := Track{
		ID:       "vid_board02",
		Title:    "Short Song",
		Artist:   "Artist Short",
		Duration: 20,
	}
	h.music.mu.Lock()
	h.music.startLocked(shortSong, "http://example.com/audio2.opus", 0, false)
	shortSeq := h.music.seq
	h.music.mu.Unlock()

	// Drain send channel
	select {
	case <-boardEdge.send:
	default:
	}

	// Send ended event
	h.music.event(boardEdge, shortSeq, shortSong.ID, "ended", "")
	time.Sleep(100 * time.Millisecond)

	hist, err = h.Store().GetHistory(ctx, admin.ID, 10, 0)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("expected 2 history entries after short song ended, got %d", len(hist))
	}
	if hist[0].VideoID != shortSong.ID || hist[0].Source != "board" {
		t.Fatalf("unexpected newest history entry: %+v", hist[0])
	}

	// Verify GET /me/history API endpoint reflects board playback
	res, body := doBearerReq(t, srv, http.MethodGet, "/hub/api/me/history", nil, token, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get history failed: %d, body: %s", res.StatusCode, string(body))
	}
	var apiHistResp struct {
		Items []struct {
			Source string `json:"source"`
			Track  Track  `json:"track"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &apiHistResp)
	if len(apiHistResp.Items) != 2 || apiHistResp.Items[0].Source != "board" {
		t.Fatalf("unexpected API history: %+v", apiHistResp)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 10: Write Rate Limiting (120 writes/min)
// ─────────────────────────────────────────────────────────────────────────────
func TestLibrary_WriteRateLimiting(t *testing.T) {
	_, srv, _, cleanup := setupTestHub(t, nil)
	defer cleanup()

	token := getAuthToken(t, srv, "admin", "AdminPassword123!")

	// 120 writes succeed
	for i := 0; i < 120; i++ {
		tr := sampleTrack(fmt.Sprintf("vid_rl_%04d", i), "Rate Limit Test")
		res, _ := doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/"+tr.ID, map[string]any{"track": tr}, token, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("write %d failed: %d", i, res.StatusCode)
		}
	}

	// 121st write must be rate limited with 429
	tr121 := sampleTrack("vid_rl__121", "Rate Limit Test")
	res, body := doBearerReq(t, srv, http.MethodPut, "/hub/api/me/likes/"+tr121.ID, map[string]any{"track": tr121}, token, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for 121st write, got %d: %s", res.StatusCode, string(body))
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &errResp)
	if errResp.Error.Code != "rate_limited" {
		t.Fatalf("expected code rate_limited, got %s", errResp.Error.Code)
	}
}
