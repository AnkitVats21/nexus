package auth

import (
	"net"
	"net/url"
	"strings"
)

// NormalizeHostPort normalizes a URL or host:port string into a lowercase host[:port] string.
// Default ports (80 for http, 443 for https) are stripped so that
// http://example.com and http://example.com:80 compare equal,
// while non-default ports (example.com:8080 vs example.com:9999) never match.
func NormalizeHostPort(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	scheme := ""
	hostPort := raw

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err == nil {
			scheme = strings.ToLower(u.Scheme)
			hostPort = u.Host
		}
	}

	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		h := strings.Trim(hostPort, "[]")
		return strings.ToLower(h)
	}

	h := strings.Trim(host, "[]")
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") || (scheme == "" && (port == "80" || port == "443")) {
		return strings.ToLower(h)
	}

	return strings.ToLower(net.JoinHostPort(h, port))
}

// MatchOrigin checks whether origin matches reqHost, publicURL, or any of allowedOrigins
// using exact scheme-less host:port comparison (normalizing default ports 80/443).
func MatchOrigin(origin string, reqHost string, publicURL string, allowedOrigins []string) bool {
	originNorm := NormalizeHostPort(origin)
	if originNorm == "" {
		return false
	}

	if reqHost != "" && originNorm == NormalizeHostPort(reqHost) {
		return true
	}

	if publicURL != "" && originNorm == NormalizeHostPort(publicURL) {
		return true
	}

	for _, allowed := range allowedOrigins {
		if allowed == "*" || (allowed != "" && originNorm == NormalizeHostPort(allowed)) {
			return true
		}
	}

	return false
}
