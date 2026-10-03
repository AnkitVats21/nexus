package auth

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
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

	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return ErrCSRFOriginMismatch
	}

	originHost := u.Host
	reqHost := r.Host

	if strings.EqualFold(originHost, reqHost) {
		return nil
	}

	// Compare hostnames without port if one has port and other does not
	originHostname := u.Hostname()
	reqHostname, _, err := net.SplitHostPort(reqHost)
	if err != nil {
		reqHostname = reqHost
	}
	if strings.EqualFold(originHostname, reqHostname) {
		return nil
	}

	// Compare with publicURL if configured
	if publicURL != "" {
		pubU, err := url.Parse(publicURL)
		if err == nil && pubU.Host != "" {
			if strings.EqualFold(originHost, pubU.Host) || strings.EqualFold(originHostname, pubU.Hostname()) {
				return nil
			}
		}
	}

	return ErrCSRFOriginMismatch
}
