package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func musicStub(t *testing.T) *MusicService {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/search":
			fmt.Fprint(w, `[{"videoId":"live1","title":"Live","author":"A","lengthSeconds":0},
				{"videoId":"abc","title":"Song","author":"Artist","lengthSeconds":200},
				{"videoId":"def","title":"Other","author":"B","lengthSeconds":180}]`)
		case r.URL.Path == "/api/v1/videos/abc":
			fmt.Fprint(w, `{"videoId":"abc","title":"Song","author":"Artist","lengthSeconds":200,
				"adaptiveFormats":[
					{"type":"video/webm; codecs=\"vp9\"","url":"https://v/vid","bitrate":900000,"qualityLabel":"720p"},
					{"type":"audio/mp4; codecs=\"mp4a.40.2\"","url":"https://v/aac","bitrate":130000},
					{"type":"audio/webm; codecs=\"opus\"","url":"https://v/opus-low","bitrate":70000},
					{"type":"audio/webm; codecs=\"opus\"","url":"https://v/opus-high","bitrate":140000}],
				"recommendedVideos":[{"videoId":"r1","title":"R1","author":"X","lengthSeconds":100},
					{"videoId":"r2","title":"R2","author":"Y","lengthSeconds":0},
					{"videoId":"r3","title":"R3","author":"Z","lengthSeconds":120}]}`)
		case r.URL.Path == "/api/v1/videos/noaudio":
			fmt.Fprint(w, `{"videoId":"noaudio","adaptiveFormats":[{"type":"audio/mp4","url":"https://v/aac","bitrate":1}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewMusicService(srv.Client(), srv.URL)
}

func TestMusicSearchSkipsLiveStreams(t *testing.T) {
	got, err := musicStub(t).Search(context.Background(), "song", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "abc" || got[0].Artist != "Artist" || got[0].Duration != 200 {
		t.Fatalf("unexpected results %+v", got)
	}
}

func TestMusicTrackPicksBestOpusAudio(t *testing.T) {
	res, err := musicStub(t).Track(context.Background(), "abc", true, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.StreamURL != "https://v/opus-high" || res.Bitrate != 140000 {
		t.Errorf("stream %q at %d, want the 140k Opus one", res.StreamURL, res.Bitrate)
	}
	if len(res.Recommendations) != 2 || res.Recommendations[1].ID != "r3" {
		t.Errorf("recommendations %+v, want r1 and r3 (r2 has no length)", res.Recommendations)
	}
}

func TestMusicTrackNotFound(t *testing.T) {
	svc := musicStub(t)
	for _, id := range []string{"gone", "noaudio"} {
		res, structured, err := svc.TrackHandler()(context.Background(), nil, &MusicTrackParams{ID: id})
		if err != nil || res.IsError {
			t.Fatalf("%s: a missing video must not be a tool error (%v)", id, err)
		}
		if r, ok := structured.(MusicTrackResult); !ok || r.Error != "not_found" || r.ID != id {
			t.Errorf("%s: structured %+v, want not_found", id, structured)
		}
	}
}

func TestMusicTrackByQuery(t *testing.T) {
	no := false
	res, structured, err := musicStub(t).TrackHandler()(context.Background(), nil,
		&MusicTrackParams{Query: "song", Stream: &no})
	if err != nil || res.IsError {
		t.Fatalf("unexpected error %v %+v", err, res)
	}
	r := structured.(MusicTrackResult)
	if r.ID != "abc" || r.StreamURL != "" {
		t.Errorf("got %+v, want abc without a stream", r)
	}
}

func TestMusicServiceUnreachableIsToolError(t *testing.T) {
	svc := NewMusicService(nil, "http://127.0.0.1:1")
	res, _, _ := svc.SearchHandler()(context.Background(), nil, &MusicSearchParams{Query: "x"})
	text, _ := res.Content[0].(*mcp.TextContent)
	if !res.IsError || text == nil || !strings.Contains(text.Text, "failed") {
		t.Errorf("want a tool error, got %+v", res)
	}
}
