package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	SessionCookieName = "nexus_session"
	SessionDuration   = 30 * 24 * time.Hour // 30 days idle sliding
)

var (
	ErrNoSessionCookie = errors.New("no session cookie found")
	ErrInvalidSession  = errors.New("invalid session token")
)

// GenerateSessionToken produces 32 cryptographically random bytes encoded as base64url,
// along with its sha256 checksum for database storage.
func GenerateSessionToken() (string, [32]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", [32]byte{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	return token, hash, nil
}

// TokenHash returns the sha256 checksum of a session token string.
func TokenHash(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// GetSessionTokenFromRequest extracts and hashes the nexus_session cookie from an HTTP request.
func GetSessionTokenFromRequest(r *http.Request) (string, [32]byte, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		return "", [32]byte{}, ErrNoSessionCookie
	}
	return c.Value, TokenHash(c.Value), nil
}

// SetSessionCookie writes the nexus_session cookie to the ResponseWriter.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt time.Time, trustedProxy string) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}

	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r, trustedProxy),
	}
	http.SetCookie(w, cookie)
}

// ClearSessionCookie invalidates the nexus_session cookie on the client.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request, trustedProxy string) {
	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r, trustedProxy),
	}
	http.SetCookie(w, cookie)
}

// IsSecureRequest determines if a request was received over HTTPS (direct TLS or via trusted proxy).
func IsSecureRequest(r *http.Request, trustedProxy string) bool {
	if r.TLS != nil {
		return true
	}
	if trustedProxy == "" {
		return false
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if host == trustedProxy {
		proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
		return proto == "https"
	}

	return false
}
