package resolver

import (
	"context"
	"fmt"
	"strings"
)

// DirectResolver delegates stream extraction directly to local mpv (via yt-dlp).
// This is optimal for PCs where yt-dlp runs quickly and no external service is needed.
type DirectResolver struct{}

// NewDirectResolver instantiates a DirectResolver.
func NewDirectResolver() *DirectResolver {
	return &DirectResolver{}
}

func (r *DirectResolver) Name() string {
	return "direct (local mpv/yt-dlp)"
}

func (r *DirectResolver) Resolve(ctx context.Context, videoID string) (*ResolvedTrack, error) {
	cleanID := strings.TrimSpace(videoID)
	if cleanID == "" {
		return nil, fmt.Errorf("empty videoId")
	}

	// mpv handles youtube.com URLs directly using its built-in yt-dlp hook
	playURL := fmt.Sprintf("https://www.youtube.com/watch?v=%s", cleanID)

	return &ResolvedTrack{
		VideoID:      cleanID,
		PlaybackURL:  playURL,
		ThumbnailURL: DefaultThumbnailURL(cleanID),
	}, nil
}
