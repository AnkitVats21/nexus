package resolver

import (
	"context"
	"fmt"
	"strings"
)

// ResolvedTrack contains resolved stream location and track metadata.
type ResolvedTrack struct {
	VideoID      string
	PlaybackURL  string // Direct HTTP audio URL or YouTube URL for mpv
	Title        string
	Artist       string
	DurationSecs int
	ThumbnailURL string
}

// Resolver defines the pluggable interface for resolving audio streams.
type Resolver interface {
	Resolve(ctx context.Context, videoID string) (*ResolvedTrack, error)
	Name() string
}

// NewResolver creates a resolver based on type: "direct", "invidious", or "auto".
func NewResolver(resolverType, invidiousBaseURL string) (Resolver, error) {
	switch strings.ToLower(resolverType) {
	case "direct", "ytdl":
		return NewDirectResolver(), nil
	case "invidious", "remote":
		if invidiousBaseURL == "" {
			return nil, fmt.Errorf("invidious resolver requires a valid baseURL")
		}
		return NewInvidiousResolver(invidiousBaseURL), nil
	case "auto", "hybrid", "":
		direct := NewDirectResolver()
		invidious := NewInvidiousResolver(invidiousBaseURL)
		return NewAutoResolver(direct, invidious), nil
	default:
		return nil, fmt.Errorf("unknown resolver type: %q (expected 'direct', 'invidious', or 'auto')", resolverType)
	}
}

// DefaultThumbnailURL formats the standard YouTube thumbnail URL.
func DefaultThumbnailURL(videoID string) string {
	return fmt.Sprintf("https://img.youtube.com/vi/%s/hqdefault.jpg", videoID)
}
