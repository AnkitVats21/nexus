package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Music lookup for the device itself (not for the voice model): search and
// stream resolution through an Invidious-compatible API (invidious-daemon).
//
// The stream URLs are signed and bound to the IP that resolved them, so the
// API must reach YouTube from the device's network: invidious-daemon does
// (its yt-dlp goes through the home proxy). This server only passes them on.

// DeviceOnlyMeta marks a tool the device calls itself: it does not offer it
// to the voice model (waveshare main/services/mcp/McpUtil.cpp).
var DeviceOnlyMeta = mcp.Meta{"nexus/device_only": true}

// MusicTrack is a song as the device's queue holds it.
type MusicTrack struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Duration int    `json:"duration"` // seconds
}

// MusicSearchParams are the arguments of music_search.
type MusicSearchParams struct {
	Query string `json:"query" jsonschema:"Song title and/or artist, or a genre"`
	Limit int    `json:"limit,omitempty" jsonschema:"Results to return (1-20, default 10)"`
}

// MusicSearchResult is music_search's structured result.
type MusicSearchResult struct {
	Tracks []MusicTrack `json:"tracks"`
}

// MusicTrackParams are the arguments of music_track: a video id, or a query
// whose top result is used (search and resolve in one call).
type MusicTrackParams struct {
	ID              string `json:"id,omitempty" jsonschema:"YouTube video id"`
	Query           string `json:"query,omitempty" jsonschema:"Search query, used when id is empty"`
	Stream          *bool  `json:"stream,omitempty" jsonschema:"Resolve the audio stream URL (default true)"`
	Recommendations int    `json:"recommendations,omitempty" jsonschema:"Related tracks to return (0-20, default 0)"`
}

// MusicTrackResult is music_track's structured result.
type MusicTrackResult struct {
	MusicTrack
	StreamURL       string       `json:"stream_url,omitempty"`
	Bitrate         int          `json:"bitrate,omitempty"`
	Recommendations []MusicTrack `json:"recommendations,omitempty"`
	// "not_found": the video is gone, private or has no audio stream (the
	// device skips it); empty on success. Other failures are tool errors
	// (the device may retry or fall back).
	Error string `json:"error,omitempty"`
}

// ErrMusicNotFound: no such video, or nothing playable in it.
var ErrMusicNotFound = errors.New("not found")

// MusicService talks to the Invidious-compatible API.
type MusicService struct {
	HTTPClient *http.Client
	BaseURL    string // e.g. https://stream.example.com (no trailing slash)
}

// NewMusicService returns a MusicService for the API at baseURL.
func NewMusicService(client *http.Client, baseURL string) *MusicService {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &MusicService{HTTPClient: client, BaseURL: strings.TrimRight(baseURL, "/")}
}

type invTrack struct {
	VideoID       string `json:"videoId"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	LengthSeconds int    `json:"lengthSeconds"`
}

func (t invTrack) toTrack() MusicTrack {
	return MusicTrack{ID: t.VideoID, Title: t.Title, Artist: t.Author, Duration: t.LengthSeconds}
}

type invVideo struct {
	invTrack
	AdaptiveFormats []struct {
		Type         string `json:"type"`
		URL          string `json:"url"`
		Bitrate      int    `json:"bitrate"`
		QualityLabel string `json:"qualityLabel"`
	} `json:"adaptiveFormats"`
	RecommendedVideos []invTrack `json:"recommendedVideos"`
}

func (s *MusicService) getJSON(ctx context.Context, path string, out any) error {
	if s.BaseURL == "" {
		return errors.New("music API not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("music API request failed: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrMusicNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("music API returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Search returns up to limit videos for the query (those without a length,
// e.g. live streams, are left out).
func (s *MusicService) Search(ctx context.Context, query string, limit int) ([]MusicTrack, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	var items []invTrack
	path := "/api/v1/search?q=" + url.QueryEscape(query) + "&type=video&fields=videoId,title,author,lengthSeconds"
	if err := s.getJSON(ctx, path, &items); err != nil {
		return nil, err
	}
	out := make([]MusicTrack, 0, limit)
	for _, it := range items {
		if it.VideoID == "" || it.LengthSeconds <= 0 {
			continue
		}
		out = append(out, it.toTrack())
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// Track resolves one video: its details, the best Opus audio stream (when
// stream is set) and up to recs related tracks.
func (s *MusicService) Track(ctx context.Context, id string, stream bool, recs int) (MusicTrackResult, error) {
	fields := "videoId,title,author,lengthSeconds"
	if stream {
		fields += ",adaptiveFormats(type,url,bitrate,qualityLabel)"
	}
	if recs > 0 {
		fields += ",recommendedVideos(videoId,title,author,lengthSeconds)"
	}
	var v invVideo
	if err := s.getJSON(ctx, "/api/v1/videos/"+url.PathEscape(id)+"?fields="+url.QueryEscape(fields), &v); err != nil {
		return MusicTrackResult{}, err
	}
	res := MusicTrackResult{MusicTrack: v.toTrack()}
	if res.ID == "" {
		res.ID = id
	}
	if stream {
		// Audio only (video formats carry a quality label), Opus (the
		// device decodes WebM/Opus), highest bitrate.
		for _, f := range v.AdaptiveFormats {
			if f.URL == "" || f.QualityLabel != "" || !strings.Contains(strings.ToLower(f.Type), "opus") {
				continue
			}
			if f.Bitrate > res.Bitrate {
				res.Bitrate, res.StreamURL = f.Bitrate, f.URL
			}
		}
		if res.StreamURL == "" {
			return MusicTrackResult{}, ErrMusicNotFound
		}
	}
	for _, r := range v.RecommendedVideos {
		if len(res.Recommendations) == recs {
			break
		}
		if r.VideoID != "" && r.LengthSeconds > 0 {
			res.Recommendations = append(res.Recommendations, r.toTrack())
		}
	}
	return res, nil
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// SearchHandler serves music_search.
func (s *MusicService) SearchHandler() func(context.Context, *mcp.CallToolRequest, *MusicSearchParams) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, p *MusicSearchParams) (*mcp.CallToolResult, any, error) {
		if p == nil || strings.TrimSpace(p.Query) == "" {
			return toolError("query is required"), nil, nil
		}
		tracks, err := s.Search(ctx, strings.TrimSpace(p.Query), p.Limit)
		if err != nil {
			return toolError("music search failed: " + err.Error()), nil, nil
		}
		res := MusicSearchResult{Tracks: tracks}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%d tracks", len(tracks))}},
		}, res, nil
	}
}

// TrackHandler serves music_track.
func (s *MusicService) TrackHandler() func(context.Context, *mcp.CallToolRequest, *MusicTrackParams) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, p *MusicTrackParams) (*mcp.CallToolResult, any, error) {
		if p == nil {
			return toolError("id or query is required"), nil, nil
		}
		id, query := strings.TrimSpace(p.ID), strings.TrimSpace(p.Query)
		stream := p.Stream == nil || *p.Stream
		recs := p.Recommendations
		if recs < 0 || recs > 20 {
			recs = 0
		}
		if id == "" {
			if query == "" {
				return toolError("id or query is required"), nil, nil
			}
			found, err := s.Search(ctx, query, 1)
			if err != nil {
				return toolError("music search failed: " + err.Error()), nil, nil
			}
			if len(found) == 0 {
				return notFound(MusicTrackResult{}), MusicTrackResult{Error: "not_found"}, nil
			}
			id = found[0].ID
		}
		res, err := s.Track(ctx, id, stream, recs)
		if errors.Is(err, ErrMusicNotFound) {
			r := MusicTrackResult{MusicTrack: MusicTrack{ID: id}, Error: "not_found"}
			return notFound(r), r, nil
		}
		if err != nil {
			return toolError("music lookup failed: " + err.Error()), nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("%s - %s", res.Artist, res.Title)}},
		}, res, nil
	}
}

// A video that is gone is an answer, not a failure: the device skips it.
func notFound(r MusicTrackResult) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "not found: " + r.ID}}}
}
