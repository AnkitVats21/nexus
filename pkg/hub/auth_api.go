package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"nexus/pkg/auth"
	"nexus/pkg/store"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return false
	}
	return true
}

func (h *Hub) authenticate(r *http.Request) (*store.User, *store.Session, error) {
	if h.auth == nil {
		return nil, nil, nil
	}
	_, hash, err := auth.GetSessionTokenFromRequest(r)
	if err != nil {
		return nil, nil, auth.ErrUnauthorized
	}
	return h.auth.AuthenticateSession(r.Context(), hash)
}

func (h *Hub) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/hub/api/", h.serveHubAPI)
}

func (h *Hub) serveHubAPI(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)

	path := strings.TrimPrefix(r.URL.Path, "/hub/api")
	if path == "" || path == "/" {
		http.NotFound(w, r)
		return
	}

	switch {
	case path == "/setup":
		if r.Method == http.MethodGet {
			h.handleSetupGet(w, r)
			return
		}
		if r.Method == http.MethodPost {
			h.handleSetupPost(w, r)
			return
		}
	case path == "/login":
		if r.Method == http.MethodPost {
			h.handleLoginPost(w, r)
			return
		}
	case path == "/logout":
		if r.Method == http.MethodPost {
			h.handleLogoutPost(w, r)
			return
		}
	case path == "/me":
		if r.Method == http.MethodGet {
			h.handleMeGet(w, r)
			return
		}
	case path == "/me/password":
		if r.Method == http.MethodPost {
			h.handleMePasswordPost(w, r)
			return
		}
	case path == "/users":
		if r.Method == http.MethodGet {
			h.handleUsersGet(w, r)
			return
		}
		if r.Method == http.MethodPost {
			h.handleUsersPost(w, r)
			return
		}
	case strings.HasPrefix(path, "/users/"):
		rest := strings.TrimPrefix(path, "/users/")
		if strings.HasSuffix(rest, "/reset") {
			idStr := strings.TrimSuffix(rest, "/reset")
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err == nil && r.Method == http.MethodPost {
				h.handleUserResetPost(w, r, id)
				return
			}
		} else {
			id, err := strconv.ParseInt(rest, 10, 64)
			if err == nil && r.Method == http.MethodDelete {
				h.handleUserDelete(w, r, id)
				return
			}
		}
	case path == "/pairing":
		if r.Method == http.MethodGet {
			h.handlePairingGet(w, r)
			return
		}
		if r.Method == http.MethodPost {
			h.handlePairingPost(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			h.handlePairingDelete(w, r)
			return
		}
	case strings.HasPrefix(path, "/pairing/"):
		code := strings.TrimPrefix(path, "/pairing/")
		if r.Method == http.MethodDelete {
			h.handlePairingCodeDelete(w, r, code)
			return
		}
	case path == "/devices":
		if r.Method == http.MethodGet {
			h.handleDevicesGet(w, r)
			return
		}
	case strings.HasPrefix(path, "/devices/"):
		id := strings.TrimPrefix(path, "/devices/")
		if r.Method == http.MethodPatch {
			h.handleDevicePatch(w, r, id)
			return
		}
		if r.Method == http.MethodDelete {
			h.handleDeviceDelete(w, r, id)
			return
		}
	}

	http.NotFound(w, r)
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/setup
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"needed": false})
		return
	}
	needed, err := h.auth.SetupNeeded(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check setup status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needed": needed})
}

type setupReq struct {
	SetupCode   string `json:"setup_code"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

func (h *Hub) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer auth.EnsureMinDuration(start, auth.AuthMinDuration)

	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	if h.auth == nil {
		writeError(w, http.StatusBadRequest, "auth not enabled")
		return
	}

	var req setupReq
	if !readJSON(w, r, &req) {
		return
	}

	ip := auth.ClientIP(r, h.cfg.TrustedProxy)
	u, token, expiresAt, err := h.auth.Setup(r.Context(), req.SetupCode, req.Username, req.DisplayName, req.Password, r.UserAgent(), ip)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "service busy, please try again")
			return
		}
		if err == auth.ErrTooManyAttempts {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	auth.SetSessionCookie(w, r, token, expiresAt, h.cfg.TrustedProxy)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/login
// ─────────────────────────────────────────────────────────────────────────────

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Hub) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer auth.EnsureMinDuration(start, auth.AuthMinDuration)

	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	if h.auth == nil {
		writeError(w, http.StatusBadRequest, "auth not enabled")
		return
	}

	var req loginReq
	if !readJSON(w, r, &req) {
		return
	}

	ip := auth.ClientIP(r, h.cfg.TrustedProxy)
	u, token, expiresAt, err := h.auth.Login(r.Context(), req.Username, req.Password, r.UserAgent(), ip)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "service busy, please try again")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	auth.SetSessionCookie(w, r, token, expiresAt, h.cfg.TrustedProxy)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/logout
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) handleLogoutPost(w http.ResponseWriter, r *http.Request) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	if h.auth == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	_, hash, err := auth.GetSessionTokenFromRequest(r)
	if err == nil {
		_ = h.auth.Logout(r.Context(), hash)
	}

	auth.ClearSessionCookie(w, r, h.cfg.TrustedProxy)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/me
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) handleMeGet(w http.ResponseWriter, r *http.Request) {
	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"role":         u.Role,
		"must_change":  u.MustChange,
	})
}

type changePasswordReq struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (h *Hub) handleMePasswordPost(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer auth.EnsureMinDuration(start, auth.AuthMinDuration)

	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, sess, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req changePasswordReq
	if !readJSON(w, r, &req) {
		return
	}

	if err := h.auth.ChangePassword(r.Context(), u.ID, req.Current, req.New, sess.IDHash); err != nil {
		if errors.Is(err, auth.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "service busy, please try again")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/users (Admin)
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) handleUsersGet(w http.ResponseWriter, r *http.Request) {
	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	if users == nil {
		users = []*store.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

type createUserReq struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func (h *Hub) handleUsersPost(w http.ResponseWriter, r *http.Request) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	var req createUserReq
	if !readJSON(w, r, &req) {
		return
	}

	if req.Role != "admin" && req.Role != "member" {
		writeError(w, http.StatusBadRequest, "role must be 'admin' or 'member'")
		return
	}
	if err := auth.ValidateUsername(req.Username); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		req.DisplayName = req.Username
	}

	tempPassword := auth.GenerateTempPassword()
	passHash, err := auth.HashPassword(tempPassword)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "service busy, please try again")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	newUser := &store.User{
		Username:    req.Username,
		DisplayName: req.DisplayName,
		Role:        req.Role,
		PassHash:    passHash,
		MustChange:  true,
	}

	if err := h.store.CreateUser(r.Context(), newUser); err != nil {
		if err == store.ErrConflict {
			writeError(w, http.StatusConflict, "username already taken")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user":          newUser,
		"temp_password": tempPassword,
	})
}

func (h *Hub) handleUserResetPost(w http.ResponseWriter, r *http.Request, targetID int64) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	targetUser, err := h.store.GetUserByID(r.Context(), targetID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get user")
		return
	}

	tempPassword := auth.GenerateTempPassword()
	passHash, err := auth.HashPassword(tempPassword)
	if err != nil {
		if errors.Is(err, auth.ErrBusy) {
			writeError(w, http.StatusServiceUnavailable, "service busy, please try again")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	if err := h.store.UpdateUserPassword(r.Context(), targetUser.ID, passHash, true); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}

	// Revoke all sessions for the target user
	_ = h.store.DeleteSessionsForUserExcept(r.Context(), targetUser.ID, nil)

	writeJSON(w, http.StatusOK, map[string]string{
		"temp_password": tempPassword,
	})
}

func (h *Hub) handleUserDelete(w http.ResponseWriter, r *http.Request, targetID int64) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}
	if u.Role != "admin" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}

	if u.ID == targetID {
		writeError(w, http.StatusBadRequest, "cannot delete own account")
		return
	}

	targetUser, err := h.store.GetUserByID(r.Context(), targetID)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get user")
		return
	}

	if targetUser.Role == "admin" {
		adminCount, err := h.store.CountAdmins(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to count admins")
			return
		}
		if adminCount <= 1 {
			writeError(w, http.StatusBadRequest, "cannot delete last admin user")
			return
		}
	}

	if err := h.store.DeleteUser(r.Context(), targetID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/pairing
// ─────────────────────────────────────────────────────────────────────────────

func (h *Hub) handlePairingGet(w http.ResponseWriter, r *http.Request) {
	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	now := time.Now().UTC().Unix()
	codes, err := h.auth.Store().ListActivePairingCodes(r.Context(), u.ID, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pairing codes")
		return
	}
	if codes == nil {
		codes = []*store.PairingCode{}
	}
	writeJSON(w, http.StatusOK, codes)
}

func (h *Hub) handlePairingPost(w http.ResponseWriter, r *http.Request) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	now := time.Now().UTC().Unix()
	count, err := h.auth.Store().CountActivePairingCodes(r.Context(), u.ID, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check active codes")
		return
	}
	if count >= auth.MaxActivePairingCodes {
		writeError(w, http.StatusBadRequest, "maximum 3 active pairing codes reached")
		return
	}

	code := auth.GeneratePairingCode()
	expiresAt := now + int64(auth.PairingCodeTTL.Seconds())
	codeHash := auth.HashPairingCode(h.pairingPepper, code)

	pc := &store.PairingCode{
		CodeHash:  codeHash,
		UserID:    u.ID,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}
	if err := h.auth.Store().CreatePairingCode(r.Context(), pc); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create pairing code")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_at": expiresAt,
	})
}

func (h *Hub) handlePairingDelete(w http.ResponseWriter, r *http.Request) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	if err := h.auth.Store().DeletePairingCodesForUser(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to cancel pairing codes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Hub) handlePairingCodeDelete(w http.ResponseWriter, r *http.Request, code string) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	clean := auth.NormalizePairingCode(code)
	codeHash := auth.HashPairingCode(h.pairingPepper, clean)
	pc, err := h.auth.Store().GetPairingCode(r.Context(), codeHash)
	if err != nil || pc.UserID != u.ID {
		writeError(w, http.StatusNotFound, "pairing code not found")
		return
	}

	_ = h.auth.Store().DeletePairingCode(r.Context(), codeHash)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// /hub/api/devices
// ─────────────────────────────────────────────────────────────────────────────

type deviceResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Online     bool   `json:"online"`
	FW         string `json:"fw"`
	Roles      any    `json:"roles,omitempty"`
	Uplink     string `json:"uplink,omitempty"`
	LastSeen   int64  `json:"last_seen"`
	ConnectedS int64  `json:"connected_s"`
	Owner      string `json:"owner"`
	Legacy     bool   `json:"legacy,omitempty"`
}

func (h *Hub) handleDevicesGet(w http.ResponseWriter, r *http.Request) {
	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	var dbDevs []*store.Device
	if u.Role == "admin" {
		dbDevs, err = h.auth.Store().ListAllDevices(r.Context())
	} else {
		dbDevs, err = h.auth.Store().ListDevicesForUser(r.Context(), u.ID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}

	userMap := make(map[int64]string)
	userMap[u.ID] = u.DisplayName

	h.mu.Lock()
	defer h.mu.Unlock()

	var result []deviceResponse
	seen := make(map[string]bool)

	for _, d := range dbDevs {
		seen[d.ID] = true
		ownerName := userMap[d.OwnerID]
		if ownerName == "" {
			if ownerUser, err := h.auth.Store().GetUserByID(r.Context(), d.OwnerID); err == nil && ownerUser != nil {
				ownerName = ownerUser.DisplayName
				userMap[d.OwnerID] = ownerName
			}
		}

		resp := deviceResponse{
			ID:       d.ID,
			Name:     d.Name,
			Kind:     d.Kind,
			FW:       d.LastFW,
			LastSeen: d.LastSeen,
			Owner:    ownerName,
		}

		e := h.edges[d.ID]
		if e != nil {
			resp.Online = true
			resp.ConnectedS = int64(time.Since(e.since).Seconds())
			resp.Roles = e.roles
			resp.Uplink = e.voiceUp
			if e.fw != "" {
				resp.FW = e.fw
			}
			if e.name != "" {
				resp.Name = e.name
			}
			if e.isLegacyToken {
				resp.Legacy = true
			}
		}
		result = append(result, resp)
	}

	// For admin or first admin: include any connected legacy edges not in DB
	firstAdmin, _ := h.auth.Store().GetFirstAdmin(r.Context())
	if u.Role == "admin" || (firstAdmin != nil && u.ID == firstAdmin.ID) {
		for id, e := range h.edges {
			if !seen[id] && e.isLegacyToken {
				seen[id] = true
				adminName := ""
				if firstAdmin != nil {
					adminName = firstAdmin.DisplayName
				}
				result = append(result, deviceResponse{
					ID:         e.id,
					Name:       e.name,
					Kind:       e.kind,
					Online:     true,
					FW:         e.fw,
					Roles:      e.roles,
					Uplink:     e.voiceUp,
					LastSeen:   e.since.Unix(),
					ConnectedS: int64(time.Since(e.since).Seconds()),
					Owner:      adminName,
					Legacy:     true,
				})
			}
		}
	}

	if result == nil {
		result = []deviceResponse{}
	}
	writeJSON(w, http.StatusOK, result)
}

type patchDeviceReq struct {
	Name string `json:"name"`
}

func (h *Hub) handleDevicePatch(w http.ResponseWriter, r *http.Request, id string) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	var req patchDeviceReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name cannot be empty")
		return
	}

	d, err := h.auth.Store().GetDeviceByID(r.Context(), id)
	if err != nil {
		h.mu.Lock()
		e := h.edges[id]
		h.mu.Unlock()
		if e != nil && e.isLegacyToken && (u.Role == "admin" || e.ownerID == u.ID) {
			h.mu.Lock()
			e.name = req.Name
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": req.Name})
			return
		}
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	if u.Role != "admin" && d.OwnerID != u.ID {
		writeError(w, http.StatusForbidden, "forbidden: not device owner")
		return
	}

	if err := h.auth.Store().UpdateDeviceName(r.Context(), id, req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update device name")
		return
	}

	h.mu.Lock()
	if e := h.edges[id]; e != nil {
		e.name = req.Name
	}
	h.mu.Unlock()

	d.Name = req.Name
	writeJSON(w, http.StatusOK, map[string]any{"id": d.ID, "name": d.Name})
}

func (h *Hub) handleDeviceDelete(w http.ResponseWriter, r *http.Request, id string) {
	if err := auth.ValidateCSRF(r, h.cfg.PublicURL); err != nil {
		writeError(w, http.StatusForbidden, "csrf validation failed")
		return
	}

	u, _, err := h.authenticate(r)
	if err != nil || u == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if u.MustChange {
		writeError(w, http.StatusForbidden, "password change required")
		return
	}

	d, err := h.auth.Store().GetDeviceByID(r.Context(), id)
	if err != nil {
		h.mu.Lock()
		e := h.edges[id]
		h.mu.Unlock()
		if e != nil && e.isLegacyToken && (u.Role == "admin" || e.ownerID == u.ID) {
			h.mu.Lock()
			delete(h.edges, id)
			if h.board == e {
				h.board = nil
				h.lastState = nil
			}
			h.mu.Unlock()
			e.conn.Close(closeRejected, "bad token")
			e.cancel()
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	if u.Role != "admin" && d.OwnerID != u.ID {
		writeError(w, http.StatusForbidden, "forbidden: not device owner")
		return
	}

	if err := h.auth.Store().DeleteDevice(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete device")
		return
	}

	// Disconnect edge if connected
	h.mu.Lock()
	e := h.edges[id]
	if e != nil {
		delete(h.edges, id)
		if h.board == e {
			h.board = nil
			h.lastState = nil
		}
	}
	h.mu.Unlock()

	if e != nil {
		e.conn.Close(closeRejected, "bad token")
		e.cancel()
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
