package auth

import (
	"errors"
	"mime"
	"net/http"
)

var (
	ErrCSRFContentType    = errors.New("csrf: Content-Type must be application/json")
	ErrCSRFMissingOrigin  = errors.New("csrf: missing Origin or Referer header")
	ErrCSRFOriginMismatch = errors.New("csrf: origin host does not match request host")
)

// ValidateCSRF validates CSRF requirements for state-changing HTTP requests:
// 1. Content-Type must be application/json
// 2. Origin (or Referer) must match the request's Host or configured publicURL
func ValidateCSRF(r *http.Request, publicURL string) error {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		// State-changing requests must be verified
	default:
		return nil
	}

	// 1. Verify Content-Type
	ct := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return ErrCSRFContentType
	}

	// 2. Verify Origin or Referer
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return ErrCSRFMissingOrigin
	}

	if !MatchOrigin(origin, r.Host, publicURL, nil) {
		return ErrCSRFOriginMismatch
	}

	return nil
}
