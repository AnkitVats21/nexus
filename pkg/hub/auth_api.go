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
