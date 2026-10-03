package resolver

import (
	"context"
	"fmt"
	"log/slog"
)

// AutoResolver tries the primary resolver and falls back to secondary if it fails.
type AutoResolver struct {
	primary   Resolver
	secondary Resolver
}

// NewAutoResolver creates an AutoResolver with primary and secondary resolvers.
func NewAutoResolver(primary, secondary Resolver) *AutoResolver {
	return &AutoResolver{
		primary:   primary,
		secondary: secondary,
	}
}

func (r *AutoResolver) Name() string {
	return fmt.Sprintf("auto (%s -> %s)", r.primary.Name(), r.secondary.Name())
}

func (r *AutoResolver) Resolve(ctx context.Context, videoID string) (*ResolvedTrack, error) {
	track, err := r.primary.Resolve(ctx, videoID)
	if err == nil {
		return track, nil
	}

	slog.Warn("Primary resolver failed, attempting fallback",
		"primary", r.primary.Name(),
		"videoID", videoID,
		"err", err,
	)

	fallbackTrack, fallbackErr := r.secondary.Resolve(ctx, videoID)
	if fallbackErr == nil {
		return fallbackTrack, nil
	}

	return nil, fmt.Errorf("both resolvers failed for %s (primary: %v, secondary: %v)", videoID, err, fallbackErr)
}
