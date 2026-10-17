package hub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nexus/pkg/store"
)

type opaqueCursor struct {
	Offset int `json:"o"`
}

func encodeCursor(offset int) string {
	data, _ := json.Marshal(opaqueCursor{Offset: offset})
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(s string) int {
	if s == "" {
		return 0
	}
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0
	}
	var c opaqueCursor
	if err := json.Unmarshal(data, &c); err != nil || c.Offset < 0 {
		return 0
	}
	return c.Offset
}

func formatETag(version int64) string {
	return fmt.Sprintf(`"%d"`, version)
}

func checkETag(r *http.Request, etag string) bool {
	match := r.Header.Get("If-None-Match")
	if match == "" {
		return false
	}
	matchTrimmed := strings.Trim(strings.TrimPrefix(match, "W/"), `"`)
	etagTrimmed := strings.Trim(strings.TrimPrefix(etag, "W/"), `"`)
	return matchTrimmed == etagTrimmed
}

func parsePaging(r *http.Request) (limit, offset int) {
	limit = 50
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
			limit = l
		}
	}
	if limit > 200 {
		limit = 200
	}
	offset = decodeCursor(r.URL.Query().Get("cursor"))
	return limit, offset
}

func (h *Hub) checkLibraryWriteRateLimit(w http.ResponseWriter, userID int64) bool {
	rl := h.pushTokenRateLimiter()
	if rl != nil {
		if !rl.LibraryWriteAllowed(userID) {
			writeErrorCode(w, http.StatusTooManyRequests, "rate_limited")
			return false
		}
		rl.RecordLibraryWrite(userID)
	}
	return true
}

func (h *Hub) authenticateLibrary(w http.ResponseWriter, r *http.Request, isWrite bool) (*store.User, bool) {
	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "must change password")
		return nil, false
	}
	if isWrite {
		if err := h.validateCSRFOrBearer(r); err != nil {
			writeError(w, http.StatusForbidden, "csrf validation failed")
			return nil, false
		}
		if !h.checkLibraryWriteRateLimit(w, u.ID) {
			return nil, false
		}
	}
	return u, true
}

// ─────────────────────────────────────────────────────────────────────────────
// Sync Helpers: GET /hub/api/me/library/versions
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) handleLibraryVersionsGet(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, false)
	if !ok {
		return
	}

	lv, err := h.store.GetLibraryVersions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get library versions")
		return
	}

	writeJSON(w, http.StatusOK, map[string]int64{
		"likes":     lv.LikesVersion,
		"playlists": lv.PlaylistsVersion,
		"history":   lv.HistoryVersion,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Likes Handlers
// ─────────────────────────────────────────────────────────────────────────────

type likeItemResponse struct {
	LikedAt int64 `json:"liked_at"`
	Track   Track `json:"track"`
}

func (h *Hub) handleLikesGet(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, false)
	if !ok {
		return
	}

	lv, err := h.store.GetLibraryVersions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get versions")
		return
	}

	etag := formatETag(lv.LikesVersion)
	if checkETag(r, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	limit, offset := parsePaging(r)
	likes, err := h.store.GetLikes(r.Context(), u.ID, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get likes")
		return
	}

	nextCursor := ""
	if len(likes) > limit {
		nextCursor = encodeCursor(offset + limit)
		likes = likes[:limit]
	}

	items := make([]likeItemResponse, len(likes))
	for i, l := range likes {
		items[i] = likeItemResponse{
			LikedAt: l.LikedAt,
			Track:   trackFromStore(l.Track),
		}
	}

	res := map[string]any{
		"items": items,
	}
	if nextCursor != "" {
		res["next_cursor"] = nextCursor
	}

	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, res)
}

func (h *Hub) handleLikesIDsGet(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, false)
	if !ok {
		return
	}

	lv, err := h.store.GetLibraryVersions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get versions")
		return
	}

	etag := formatETag(lv.LikesVersion)
	if checkETag(r, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	ids, err := h.store.GetLikedIDs(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get liked ids")
		return
	}
	if ids == nil {
		ids = []string{}
	}

	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, map[string]any{
		"ids": ids,
	})
}

func (h *Hub) handleLikePut(w http.ResponseWriter, r *http.Request, videoID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var body struct {
		Track *Track `json:"track"`
		// Fallbacks in case fields are top-level
		ID        string `json:"id"`
		Title     string `json:"title"`
		Artist    string `json:"artist"`
		Album     string `json:"album"`
		Duration  int    `json:"duration"`
		Thumbnail string `json:"thumbnail"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	var t Track
	if body.Track != nil {
		t = *body.Track
	} else {
		t = Track{
			ID:        body.ID,
			Title:     body.Title,
			Artist:    body.Artist,
			Album:     body.Album,
			Duration:  body.Duration,
			Thumbnail: body.Thumbnail,
		}
	}

	if t.ID == "" {
		t.ID = videoID
	}
	if t.ID != videoID || !ValidateTrack(t) {
		writeErrorCode(w, http.StatusBadRequest, "invalid_track")
		return
	}

	nowMs := time.Now().UTC().UnixMilli()
	if err := h.store.PutLike(r.Context(), u.ID, t.toStore(), nowMs); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to put like")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Hub) handleLikeDelete(w http.ResponseWriter, r *http.Request, videoID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	if err := h.store.DeleteLike(r.Context(), u.ID, videoID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete like")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────────────────────────────────────────────────────
// Playlists Handlers
// ─────────────────────────────────────────────────────────────────────────────

type playlistResponse struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Count      int      `json:"count"`
	Version    int64    `json:"version"`
	CreatedAt  int64    `json:"created_at"`
	UpdatedAt  int64    `json:"updated_at"`
	Thumbnails []string `json:"thumbnails"`
}

type playlistItemResponse struct {
	ItemID   string `json:"item_id"`
	Position int    `json:"position"`
	AddedAt  int64  `json:"added_at"`
	Track    Track  `json:"track"`
}

func (h *Hub) handlePlaylistsGet(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, false)
	if !ok {
		return
	}

	lv, err := h.store.GetLibraryVersions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get versions")
		return
	}

	etag := formatETag(lv.PlaylistsVersion)
	if checkETag(r, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	playlists, err := h.store.GetPlaylists(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get playlists")
		return
	}

	items := make([]playlistResponse, len(playlists))
	for i, pl := range playlists {
		thumbs := pl.Thumbnails
		if thumbs == nil {
			thumbs = []string{}
		}
		items[i] = playlistResponse{
			ID:         pl.ID,
			Name:       pl.Name,
			Count:      pl.Count,
			Version:    pl.Version,
			CreatedAt:  pl.CreatedAt,
			UpdatedAt:  pl.UpdatedAt,
			Thumbnails: thumbs,
		}
	}

	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
	})
}

func (h *Hub) handlePlaylistCreate(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	trimmed := strings.TrimSpace(req.Name)
	if len(trimmed) < 1 {
		writeError(w, http.StatusBadRequest, "playlist name cannot be empty")
		return
	}
	if len(trimmed) > 100 {
		writeErrorCode(w, http.StatusUnprocessableEntity, "limit")
		return
	}

	pl := &store.Playlist{
		UserID: u.ID,
		Name:   trimmed,
	}
	if err := h.store.CreatePlaylist(r.Context(), pl); err != nil {
		if errors.Is(err, store.ErrLimitExceeded) {
			writeErrorCode(w, http.StatusUnprocessableEntity, "limit")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create playlist")
		return
	}

	writeJSON(w, http.StatusCreated, playlistResponse{
		ID:         pl.ID,
		Name:       pl.Name,
		Count:      0,
		Version:    pl.Version,
		CreatedAt:  pl.CreatedAt,
		UpdatedAt:  pl.UpdatedAt,
		Thumbnails: []string{},
	})
}

func (h *Hub) routePlaylistEndpoints(w http.ResponseWriter, r *http.Request, rest string) {
	// rest can be:
	// - "{id}" (GET, PATCH, DELETE)
	// - "{id}/items" (POST)
	// - "{id}/items/{item_id}" (DELETE)
	// - "{id}/move" (POST)
	parts := strings.Split(rest, "/")
	playlistID := parts[0]
	if playlistID == "" {
		http.NotFound(w, r)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.handlePlaylistGet(w, r, playlistID)
		case http.MethodPatch:
			h.handlePlaylistPatch(w, r, playlistID)
		case http.MethodDelete:
			h.handlePlaylistDelete(w, r, playlistID)
		default:
			http.NotFound(w, r)
		}
		return
	}

	if len(parts) == 2 {
		sub := parts[1]
		if sub == "items" && r.Method == http.MethodPost {
			h.handlePlaylistItemsAdd(w, r, playlistID)
			return
		}
		if sub == "move" && r.Method == http.MethodPost {
			h.handlePlaylistItemMove(w, r, playlistID)
			return
		}
		http.NotFound(w, r)
		return
	}

	if len(parts) == 3 && parts[1] == "items" && r.Method == http.MethodDelete {
		itemID := parts[2]
		h.handlePlaylistItemDelete(w, r, playlistID, itemID)
		return
	}

	http.NotFound(w, r)
}

func (h *Hub) handlePlaylistGet(w http.ResponseWriter, r *http.Request, playlistID string) {
	u, ok := h.authenticateLibrary(w, r, false)
	if !ok {
		return
	}

	pl, err := h.store.GetPlaylist(r.Context(), u.ID, playlistID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "playlist not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get playlist")
		return
	}

	etag := formatETag(pl.Version)
	if checkETag(r, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	limit, offset := parsePaging(r)
	items, _, err := h.store.GetPlaylistItems(r.Context(), u.ID, playlistID, limit+1, offset)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "playlist not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get playlist items")
		return
	}

	nextCursor := ""
	if len(items) > limit {
		nextCursor = encodeCursor(offset + limit)
		items = items[:limit]
	}

	itemResps := make([]playlistItemResponse, len(items))
	for i, it := range items {
		itemResps[i] = playlistItemResponse{
			ItemID:   it.ItemID,
			Position: it.Position,
			AddedAt:  it.AddedAt,
			Track:    trackFromStore(it.Track),
		}
	}

	res := map[string]any{
		"id":         pl.ID,
		"name":       pl.Name,
		"version":    pl.Version,
		"items":      itemResps,
		"created_at": pl.CreatedAt,
		"updated_at": pl.UpdatedAt,
	}
	if nextCursor != "" {
		res["next_cursor"] = nextCursor
	}

	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, res)
}

func (h *Hub) handlePlaylistPatch(w http.ResponseWriter, r *http.Request, playlistID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var req struct {
		Name    *string `json:"name"`
		Version *int64  `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	if req.Name == nil {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	trimmed := strings.TrimSpace(*req.Name)
	if len(trimmed) < 1 {
		writeError(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	if len(trimmed) > 100 {
		writeErrorCode(w, http.StatusUnprocessableEntity, "limit")
		return
	}

	newVer, err := h.store.UpdatePlaylist(r.Context(), u.ID, playlistID, trimmed, req.Version, time.Now().UTC().Unix())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "playlist not found")
			return
		}
		if errors.Is(err, store.ErrVersionConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           map[string]string{"code": "version_conflict"},
				"code":            "version_conflict",
				"current_version": newVer,
				"version":         newVer,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update playlist")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      playlistID,
		"name":    trimmed,
		"version": newVer,
	})
}

func (h *Hub) handlePlaylistDelete(w http.ResponseWriter, r *http.Request, playlistID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var expVer *int64
	if vStr := r.URL.Query().Get("version"); vStr != "" {
		if v, err := strconv.ParseInt(vStr, 10, 64); err == nil {
			expVer = &v
		}
	}

	err := h.store.DeletePlaylist(r.Context(), u.ID, playlistID, expVer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "playlist not found")
			return
		}
		if errors.Is(err, store.ErrVersionConflict) {
			writeErrorCode(w, http.StatusConflict, "version_conflict")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete playlist")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) handlePlaylistItemsAdd(w http.ResponseWriter, r *http.Request, playlistID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var req struct {
		Tracks   []Track `json:"tracks"`
		Position *int    `json:"position"`
		Version  *int64  `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	if len(req.Tracks) == 0 {
		writeError(w, http.StatusBadRequest, "tracks cannot be empty")
		return
	}

	storeTracks := make([]store.Track, len(req.Tracks))
	for i, t := range req.Tracks {
		if !ValidateTrack(t) {
			writeErrorCode(w, http.StatusBadRequest, "invalid_track")
			return
		}
		storeTracks[i] = t.toStore()
	}

	newVer, err := h.store.AddPlaylistItems(r.Context(), u.ID, playlistID, storeTracks, req.Position, req.Version, time.Now().UTC().Unix())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "playlist not found")
			return
		}
		if errors.Is(err, store.ErrVersionConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           map[string]string{"code": "version_conflict"},
				"code":            "version_conflict",
				"current_version": newVer,
				"version":         newVer,
			})
			return
		}
		if errors.Is(err, store.ErrLimitExceeded) {
			writeErrorCode(w, http.StatusUnprocessableEntity, "limit")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to add playlist items")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": newVer,
		"added":   len(req.Tracks),
	})
}

func (h *Hub) handlePlaylistItemDelete(w http.ResponseWriter, r *http.Request, playlistID, itemID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var expVer *int64
	if vStr := r.URL.Query().Get("version"); vStr != "" {
		if v, err := strconv.ParseInt(vStr, 10, 64); err == nil {
			expVer = &v
		}
	}

	newVer, err := h.store.DeletePlaylistItem(r.Context(), u.ID, playlistID, itemID, expVer, time.Now().UTC().Unix())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		if errors.Is(err, store.ErrVersionConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           map[string]string{"code": "version_conflict"},
				"code":            "version_conflict",
				"current_version": newVer,
				"version":         newVer,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete playlist item")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": newVer,
	})
}

func (h *Hub) handlePlaylistItemMove(w http.ResponseWriter, r *http.Request, playlistID string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var req struct {
		ItemID  string `json:"item_id"`
		To      int    `json:"to"`
		Version *int64 `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	if req.ItemID == "" {
		writeError(w, http.StatusBadRequest, "item_id is required")
		return
	}

	newVer, err := h.store.MovePlaylistItem(r.Context(), u.ID, playlistID, req.ItemID, req.To, req.Version, time.Now().UTC().Unix())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		if errors.Is(err, store.ErrVersionConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":           map[string]string{"code": "version_conflict"},
				"code":            "version_conflict",
				"current_version": newVer,
				"version":         newVer,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to move playlist item")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": newVer,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// History Handlers
// ─────────────────────────────────────────────────────────────────────────────

type historyItemResponse struct {
	ID       int64   `json:"id"`
	PlayedAt int64   `json:"played_at"`
	PlayedMs int64   `json:"played_ms"`
	DeviceID *string `json:"device_id,omitempty"`
	Source   string  `json:"source"`
	Track    Track   `json:"track"`
}

func (h *Hub) handleHistoryPost(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	var req struct {
		Entries []struct {
			Track    Track   `json:"track"`
			PlayedAt int64   `json:"played_at"`
			PlayedMs int64   `json:"played_ms"`
			DeviceID *string `json:"device_id"`
		} `json:"entries"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	if len(req.Entries) > 200 {
		writeErrorCode(w, http.StatusUnprocessableEntity, "limit")
		return
	}

	nowMs := time.Now().UTC().UnixMilli()
	maxFuture := nowMs + 5*60*1000
	maxPast := nowMs - 30*24*60*60*1000

	entries := make([]store.HistoryEntry, len(req.Entries))
	for i, e := range req.Entries {
		if !ValidateTrack(e.Track) {
			writeErrorCode(w, http.StatusBadRequest, "invalid_track")
			return
		}
		if e.PlayedAt > maxFuture || e.PlayedAt < maxPast {
			writeErrorCode(w, http.StatusBadRequest, "invalid_timestamp")
			return
		}

		var devID *string
		if e.DeviceID != nil && *e.DeviceID != "" {
			d, err := h.store.GetDeviceByID(r.Context(), *e.DeviceID)
			if err == nil && d != nil && d.OwnerID == u.ID {
				devID = e.DeviceID
			}
		}

		entries[i] = store.HistoryEntry{
			Track:    e.Track.toStore(),
			PlayedAt: e.PlayedAt,
			PlayedMs: e.PlayedMs,
			DeviceID: devID,
			Source:   "app",
		}
	}

	accepted, err := h.store.AddHistory(r.Context(), u.ID, entries)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record history")
		return
	}

	writeJSON(w, http.StatusOK, map[string]int{
		"accepted": accepted,
	})
}

func (h *Hub) handleHistoryGet(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, false)
	if !ok {
		return
	}

	lv, err := h.store.GetLibraryVersions(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get versions")
		return
	}

	etag := formatETag(lv.HistoryVersion)
	if checkETag(r, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	limit, offset := parsePaging(r)
	entries, err := h.store.GetHistory(r.Context(), u.ID, limit+1, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get history")
		return
	}

	nextCursor := ""
	if len(entries) > limit {
		nextCursor = encodeCursor(offset + limit)
		entries = entries[:limit]
	}

	items := make([]historyItemResponse, len(entries))
	for i, e := range entries {
		items[i] = historyItemResponse{
			ID:       e.ID,
			PlayedAt: e.PlayedAt,
			PlayedMs: e.PlayedMs,
			DeviceID: e.DeviceID,
			Source:   e.Source,
			Track:    trackFromStore(e.Track),
		}
	}

	res := map[string]any{
		"items": items,
	}
	if nextCursor != "" {
		res["next_cursor"] = nextCursor
	}

	w.Header().Set("ETag", etag)
	writeJSON(w, http.StatusOK, res)
}

func (h *Hub) handleHistoryClear(w http.ResponseWriter, r *http.Request) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	if err := h.store.ClearHistory(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to clear history")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) handleHistoryItemDelete(w http.ResponseWriter, r *http.Request, idStr string) {
	u, ok := h.authenticateLibrary(w, r, true)
	if !ok {
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid history id")
		return
	}

	if err := h.store.DeleteHistoryItem(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "history item not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete history item")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
