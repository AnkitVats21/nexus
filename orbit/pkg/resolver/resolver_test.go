package resolver

import (
	"context"
	"testing"
)

func TestDirectResolver(t *testing.T) {
	r := NewDirectResolver()
	track, err := r.Resolve(context.Background(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	if track.PlaybackURL != expected {
		t.Errorf("expected %s, got %s", expected, track.PlaybackURL)
	}
	if track.VideoID != "dQw4w9WgXcQ" {
		t.Errorf("expected dQw4w9WgXcQ, got %s", track.VideoID)
	}
}

func TestSelectBestAudioFormat(t *testing.T) {
	formats := []invidiousAdaptiveFormat{
		{
			Type:    "video/mp4",
			URL:     "http://example.com/video.mp4",
			Bitrate: 1000000,
		},
		{
			Type:    "audio/mp4; codecs=\"mp4a.40.2\"",
			URL:     "http://example.com/audio.m4a",
			Bitrate: 128000,
		},
		{
			Type:    "audio/webm; codecs=\"opus\"",
			URL:     "http://example.com/audio.opus",
			Bitrate: 160000,
		},
	}

	best := selectBestAudioFormat(formats)
	if best != "http://example.com/audio.opus" {
		t.Errorf("expected opus format to be selected, got %s", best)
	}
}
