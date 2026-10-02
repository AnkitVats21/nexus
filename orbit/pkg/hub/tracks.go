package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Track is a song as the queue holds it (docs/PROTOCOL.md, music_play).
type Track struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"` // seconds
}

// TrackInfo is one looked-up song: its details, the audio stream URL (when
// asked for) and related songs.
type TrackInfo struct {
	Track
	URL  string
	Recs []Track
}

// ErrTrackNotFound: nothing matches, or the video is gone or has no audio
// (skip it). Other errors are transient.
var ErrTrackNotFound = errors.New("track not found")

// TrackSource is where the hub looks songs up. Stream URLs are signed and
// bound to the IP that resolved them; the source must reach YouTube from the
// home network (invidious-daemon does), so the edges can play them.
type TrackSource interface {
	Search(ctx context.Context, query string, limit int) ([]Track, error)
	// Lookup resolves the video id, or the top result for query when id is
	// empty: details, the stream URL when stream is set, up to recs related.
	Lookup(ctx context.Context, id, query string, stream bool, recs int) (TrackInfo, error)
	Name() string
}

// ─────────────────────────────────────────────────────────────────────────────
// MCP: nexus-mcp's device-only music tools, as the board uses them.

type MCPTracks struct{ C *MCPClient }

func (s MCPTracks) Name() string { return "mcp" }

type mcpTrackResult struct {
	Track
	StreamURL       string  `json:"stream_url"`
	Recommendations []Track `json:"recommendations"`
	Error           string  `json:"error"`
}

func (s MCPTracks) Search(ctx context.Context, query string, limit int) ([]Track, error) {
	args, _ := json.Marshal(map[string]any{"query": query, "limit": limit})
	var res struct {
		Tracks []Track `json:"tracks"`
	}
	if err := s.C.CallStructured(ctx, "music_search", args, &res); err != nil {
		return nil, err
	}
	if len(res.Tracks) == 0 {
		return nil, ErrTrackNotFound
	}
	return res.Tracks, nil
}

func (s MCPTracks) Lookup(ctx context.Context, id, query string, stream bool, recs int) (TrackInfo, error) {
	a := map[string]any{"stream": stream, "recommendations": recs}
	if id != "" {
		a["id"] = id
	} else {
		a["query"] = query
	}
	args, _ := json.Marshal(a)
	var res mcpTrackResult
	if err := s.C.CallStructured(ctx, "music_track", args, &res); err != nil {
		return TrackInfo{}, err
	}
	if res.Error == "not_found" || res.ID == "" || (stream && res.StreamURL == "") {
		return TrackInfo{}, ErrTrackNotFound
	}
	return TrackInfo{Track: res.Track, URL: res.StreamURL, Recs: res.Recommendations}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Invidious-compatible API (invidious-daemon), as nexus-mcp calls it.

type InvidiousTracks struct {
	BaseURL string
	HTTP    *http.Client
}

func NewInvidiousTracks(baseURL string) *InvidiousTracks {
	return &InvidiousTracks{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (s *InvidiousTracks) Name() string { return "invidious" }

type invTrack struct {
	VideoID       string `json:"videoId"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	LengthSeconds int    `json:"lengthSeconds"`
}

func (t invTrack) track() Track {
	return Track{ID: t.VideoID, Title: t.Title, Artist: t.Author, Duration: t.LengthSeconds}
}

func (s *InvidiousTracks) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("music API: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrTrackNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("music API: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *InvidiousTracks) Search(ctx context.Context, query string, limit int) ([]Track, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	var items []invTrack
	if err := s.get(ctx, "/api/v1/search?q="+url.QueryEscape(query)+"&type=video&fields=videoId,title,author,lengthSeconds", &items); err != nil {
		return nil, err
	}
	var out []Track
	for _, it := range items {
		// No length: a live stream.
		if it.VideoID != "" && it.LengthSeconds > 0 {
			out = append(out, it.track())
			if len(out) == limit {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, ErrTrackNotFound
	}
	return out, nil
}

func (s *InvidiousTracks) Lookup(ctx context.Context, id, query string, stream bool, recs int) (TrackInfo, error) {
	if id == "" {
		found, err := s.Search(ctx, query, 1)
		if err != nil {
			return TrackInfo{}, err
		}
		id = found[0].ID
	}
	fields := "videoId,title,author,lengthSeconds"
	if stream {
		fields += ",adaptiveFormats(type,url,bitrate,qualityLabel)"
	}
	if recs > 0 {
		fields += ",recommendedVideos(videoId,title,author,lengthSeconds)"
	}
	var v struct {
		invTrack
		AdaptiveFormats []struct {
			Type         string `json:"type"`
			URL          string `json:"url"`
			Bitrate      int    `json:"bitrate"`
			QualityLabel string `json:"qualityLabel"`
		} `json:"adaptiveFormats"`
		RecommendedVideos []invTrack `json:"recommendedVideos"`
	}
	if err := s.get(ctx, "/api/v1/videos/"+url.PathEscape(id)+"?fields="+url.QueryEscape(fields), &v); err != nil {
		return TrackInfo{}, err
	}
	info := TrackInfo{Track: v.track()}
	if info.ID == "" {
		info.ID = id
	}
	if stream {
		// Audio only (video formats have a quality label), Opus (the board
		// decodes WebM/Opus), highest bitrate.
		best := 0
		for _, f := range v.AdaptiveFormats {
			if f.URL != "" && f.QualityLabel == "" && strings.Contains(strings.ToLower(f.Type), "opus") && f.Bitrate > best {
				best, info.URL = f.Bitrate, f.URL
			}
		}
		if info.URL == "" {
			return TrackInfo{}, ErrTrackNotFound
		}
	}
	for _, r := range v.RecommendedVideos {
		if len(info.Recs) == recs {
			break
		}
		if r.VideoID != "" && r.LengthSeconds > 0 {
			info.Recs = append(info.Recs, r.track())
		}
	}
	return info, nil
}

// ─────────────────────────────────────────────────────────────────────────────

// FallbackTracks asks Primary, then Secondary when Primary fails for a reason
// other than "not found".
type FallbackTracks struct{ Primary, Secondary TrackSource }

func (s FallbackTracks) Name() string { return s.Primary.Name() + "+" + s.Secondary.Name() }

func (s FallbackTracks) Search(ctx context.Context, query string, limit int) ([]Track, error) {
	out, err := s.Primary.Search(ctx, query, limit)
	if err != nil && !errors.Is(err, ErrTrackNotFound) {
		return s.Secondary.Search(ctx, query, limit)
	}
	return out, err
}

func (s FallbackTracks) Lookup(ctx context.Context, id, query string, stream bool, recs int) (TrackInfo, error) {
	out, err := s.Primary.Lookup(ctx, id, query, stream, recs)
	if err != nil && !errors.Is(err, ErrTrackNotFound) {
		return s.Secondary.Lookup(ctx, id, query, stream, recs)
	}
	return out, err
}
