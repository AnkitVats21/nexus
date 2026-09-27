package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type invidiousAdaptiveFormat struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Bitrate   int    `json:"bitrate"`
	Itag      string `json:"itag"`
	Container string `json:"container,omitempty"`
}

type invidiousVideoDetails struct {
	VideoID         string                    `json:"videoId"`
	Title           string                    `json:"title"`
	Author          string                    `json:"author"`
	LengthSeconds   int                       `json:"lengthSeconds"`
	AdaptiveFormats []invidiousAdaptiveFormat `json:"adaptiveFormats"`
	Error           string                    `json:"error,omitempty"`
}

// InvidiousResolver queries an invidious-daemon instance (e.g. stream.ankitm.xyz)
// to extract direct CDN audio stream URLs without high local CPU usage.
type InvidiousResolver struct {
	baseURL string
	client  *http.Client
}

// NewInvidiousResolver instantiates an InvidiousResolver.
func NewInvidiousResolver(baseURL string) *InvidiousResolver {
	cleanURL := strings.TrimRight(baseURL, "/")
	return &InvidiousResolver{
		baseURL: cleanURL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (r *InvidiousResolver) Name() string {
	return fmt.Sprintf("invidious (%s)", r.baseURL)
}

func (r *InvidiousResolver) Resolve(ctx context.Context, videoID string) (*ResolvedTrack, error) {
	cleanID := strings.TrimSpace(videoID)
	if cleanID == "" {
		return nil, fmt.Errorf("empty videoId")
	}

	reqURL := fmt.Sprintf("%s/api/v1/videos/%s", r.baseURL, cleanID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invidious request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("invidious returned HTTP %d for videoId %s", resp.StatusCode, cleanID)
	}

	var details invidiousVideoDetails
	if err := json.NewDecoder(resp.Body).Decode(&details); err != nil {
		return nil, fmt.Errorf("failed to decode invidious response: %w", err)
	}

	if details.Error != "" {
		return nil, fmt.Errorf("invidious error: %s", details.Error)
	}

	selectedURL := selectBestAudioFormat(details.AdaptiveFormats)
	if selectedURL == "" {
		return nil, fmt.Errorf("no suitable audio stream found in %d formats", len(details.AdaptiveFormats))
	}

	return &ResolvedTrack{
		VideoID:      cleanID,
		PlaybackURL:  selectedURL,
		Title:        details.Title,
		Artist:       details.Author,
		DurationSecs: details.LengthSeconds,
		ThumbnailURL: DefaultThumbnailURL(cleanID),
	}, nil
}

// selectBestAudioFormat chooses the best audio stream, prioritizing Opus over AAC.
func selectBestAudioFormat(formats []invidiousAdaptiveFormat) string {
	var bestOpusURL string
	var bestOpusBitrate int

	var bestOtherURL string
	var bestOtherBitrate int

	for _, f := range formats {
		isAudio := strings.HasPrefix(f.Type, "audio/") || f.Container == "webm" || f.Container == "m4a"
		if !isAudio {
			continue
		}

		isOpus := strings.Contains(strings.ToLower(f.Type), "opus") || f.Itag == "251" || f.Itag == "250" || f.Itag == "249"

		if isOpus {
			if f.Bitrate > bestOpusBitrate || bestOpusURL == "" {
				bestOpusBitrate = f.Bitrate
				bestOpusURL = f.URL
			}
		} else {
			if f.Bitrate > bestOtherBitrate || bestOtherURL == "" {
				bestOtherBitrate = f.Bitrate
				bestOtherURL = f.URL
			}
		}
	}

	if bestOpusURL != "" {
		return bestOpusURL
	}
	return bestOtherURL
}
