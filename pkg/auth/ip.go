package auth

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP extracts the client IP address from a request, stripping any port.
// If the direct remote host matches trustedProxy, the rightmost address in
// the X-Forwarded-For header is used instead.
func ClientIP(r *http.Request, trustedProxy string) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = strings.TrimSpace(r.RemoteAddr)
	}

	tpHost, _, err := net.SplitHostPort(trustedProxy)
	if err != nil {
		tpHost = strings.TrimSpace(trustedProxy)
	}

	if tpHost != "" && remoteHost == tpHost {
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			parts := strings.Split(xff, ",")
			last := strings.TrimSpace(parts[len(parts)-1])
			if last != "" {
				if h, _, err := net.SplitHostPort(last); err == nil {
					return h
				}
				return last
			}
		}
	}

	return remoteHost
}
