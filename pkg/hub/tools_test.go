package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nexus/pkg/tools"
)

// fakeTools records the calls it gets.
type fakeTools struct {
	name  string
	have  map[string]bool
	decls []map[string]any
	calls []string
}

func (f *fakeTools) Declarations(_ context.Context, skip map[string]bool, _ bool) []map[string]any {
	var out []map[string]any
	for _, d := range f.decls {
		if !skip[d["name"].(string)] {
			out = append(out, d)
		}
	}
	return out
}
func (f *fakeTools) Has(n string) bool { return f.have[n] }
func (f *fakeTools) Call(_ context.Context, n string, _ json.RawMessage) json.RawMessage {
	f.calls = append(f.calls, n)
	return json.RawMessage(`{"result":"` + f.name + `"}`)
}
func (f *fakeTools) CallStructured(_ context.Context, n string, _ json.RawMessage, _ any) error {
	f.calls = append(f.calls, n)
	return errors.New(f.name)
}

func decl(n string) map[string]any { return map[string]any{"name": n} }

func names(ds []map[string]any) string {
	var out []string
	for _, d := range ds {
		out = append(out, d["name"].(string))
	}
	return strings.Join(out, ",")
}

func TestToolSetRouting(t *testing.T) {
	ctx := context.Background()
	newSet := func() (ToolSet, *fakeTools, *fakeTools) {
		local := &fakeTools{name: "local", have: map[string]bool{"weather": true, "music_track": true},
			decls: []map[string]any{decl("weather")}}
		remote := &fakeTools{name: "remote", have: map[string]bool{"weather": true, "get_news": true},
			decls: []map[string]any{decl("weather"), decl("get_news")}}
		return ToolSet{Local: local, Remote: remote}, local, remote
	}

	t.Run("local hit", func(t *testing.T) {
		s, local, remote := newSet()
		if got := string(s.Call(ctx, "weather", nil)); got != `{"result":"local"}` {
			t.Fatalf("got %s", got)
		}
		if len(local.calls) != 1 || len(remote.calls) != 0 {
			t.Fatalf("local %v remote %v", local.calls, remote.calls)
		}
		if err := s.CallStructured(ctx, "music_track", nil, nil); err == nil || err.Error() != "local" {
			t.Fatalf("structured went to %v", err)
		}
	})
	t.Run("remote-only name", func(t *testing.T) {
		s, local, remote := newSet()
		if got := string(s.Call(ctx, "get_news", nil)); got != `{"result":"remote"}` {
			t.Fatalf("got %s", got)
		}
		if len(local.calls) != 0 || len(remote.calls) != 1 {
			t.Fatalf("local %v remote %v", local.calls, remote.calls)
		}
		if !s.Has("get_news") || s.Has("nothing") {
			t.Fatal("Has")
		}
	})
	t.Run("declarations: local first, remote shadowed", func(t *testing.T) {
		s, _, _ := newSet()
		if got := names(s.Declarations(ctx, nil, false)); got != "weather,get_news" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("local disabled", func(t *testing.T) {
		_, _, remote := newSet()
		s := ToolSet{Remote: remote}
		if got := string(s.Call(ctx, "weather", nil)); got != `{"result":"remote"}` {
			t.Fatalf("got %s", got)
		}
		if got := names(s.Declarations(ctx, nil, false)); got != "weather,get_news" {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("none at all", func(t *testing.T) {
		s := ToolSet{}
		if s.Has("weather") || len(s.Declarations(ctx, nil, false)) != 0 {
			t.Fatal("has tools")
		}
		if got := string(s.Call(ctx, "weather", nil)); !strings.Contains(got, `"status":"error"`) {
			t.Fatalf("got %s", got)
		}
	})
}

func TestToolSetCapsDeclarations(t *testing.T) {
	local := &fakeTools{have: map[string]bool{}}
	for _, n := range []string{"a", "b", "c"} {
		local.decls = append(local.decls, decl(n))
	}
	got := ToolSet{Local: local, MaxTools: 2}.Declarations(context.Background(), nil, false)
	if names(got) != "a,b" {
		t.Fatalf("got %s", names(got))
	}
}

// stubRegistry: real tools over stub backends (no external network).
func stubRegistry(t *testing.T, keyFile string) (*tools.Registry, *httptest.Server) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/search"):
			w.Write([]byte(`[{"videoId":"abc","title":"Song","author":"Band","lengthSeconds":200}]`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/videos/"):
			w.Write([]byte(`{"videoId":"abc","title":"Song","author":"Band","lengthSeconds":200,` +
				`"adaptiveFormats":[{"type":"audio/webm; codecs=\"opus\"","url":"http://stream.test/a","bitrate":128000}]}`))
		case strings.HasPrefix(r.URL.Path, "/gemini"):
			// A failing backend that echoes the request, header included.
			http.Error(w, "bad request: "+r.Header.Get("x-goog-api-key"), http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	music := tools.NewMusicService(api.Client(), api.URL)
	search := tools.NewSearchService(api.Client(), "", keyFile)
	search.Endpoint = api.URL + "/gemini"
	return tools.NewRegistry(nil, nil, search, music), api
}

func TestLocalTools(t *testing.T) {
	ctx := context.Background()
	reg, _ := stubRegistry(t, "")
	l := NewLocalTools(reg, nil)

	if got := strings.Join(l.Names(), ","); got != "news_headlines,web_search,weather,music_search,music_track" {
		t.Fatalf("names %s", got)
	}
	// Device-only tools are hidden from Gemini; web_search goes with builtin search.
	if got := names(l.Declarations(ctx, nil, false)); got != "news_headlines,web_search,weather" {
		t.Fatalf("declarations %s", got)
	}
	if got := names(l.Declarations(ctx, map[string]bool{"weather": true}, true)); got != "news_headlines" {
		t.Fatalf("declarations %s", got)
	}
	for _, d := range l.Declarations(ctx, nil, false) {
		if d["parametersJsonSchema"] == nil {
			t.Fatalf("%v: no schema", d["name"])
		}
	}

	// The device-only tool still answers a call, structured.
	var res struct {
		Tracks []Track `json:"tracks"`
	}
	if err := l.CallStructured(ctx, "music_search", json.RawMessage(`{"query":"song"}`), &res); err != nil || len(res.Tracks) != 1 || res.Tracks[0].ID != "abc" {
		t.Fatalf("music_search: %v %+v", err, res)
	}
	out := l.Call(ctx, "music_search", json.RawMessage(`{"query":"song"}`))
	var wrapped struct {
		Result            string          `json:"result"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(out, &wrapped); err != nil || wrapped.Result != "1 tracks" || len(wrapped.StructuredContent) == 0 {
		t.Fatalf("Call: %s", out)
	}
	// A tool error and bad arguments come back in the board's error shape.
	for _, args := range []string{`{"query":""}`, `{"query":5}`} {
		if got := string(l.Call(ctx, "music_search", json.RawMessage(args))); !strings.Contains(got, `"status":"error"`) {
			t.Fatalf("%s -> %s", args, got)
		}
	}
	if err := l.CallStructured(ctx, "music_search", json.RawMessage(`{"query":""}`), &res); err == nil {
		t.Fatal("tool error must be an error")
	}
	if got := string(l.Call(ctx, "nope", nil)); !strings.Contains(got, "no such tool") {
		t.Fatalf("got %s", got)
	}
}

func TestLocalToolErrorsCarryNoKey(t *testing.T) {
	const secret = "SECRET-KEY-VALUE"
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, _ := stubRegistry(t, keyFile)
	l := NewLocalTools(reg, nil)
	// The stub echoes the key header into its error body; whatever the tool
	// returns, check it here: the key must not reach the model.
	got := string(l.Call(context.Background(), "web_search", json.RawMessage(`{"query":"x"}`)))
	if !strings.Contains(got, `"status":"error"`) {
		t.Fatalf("expected an error, got %s", got)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("key leaked into the tool error: %s", got)
	}
	// And a key file that is missing says so without a key.
	reg2, _ := stubRegistry(t, filepath.Join(t.TempDir(), "absent"))
	got = string(NewLocalTools(reg2, nil).Call(context.Background(), "web_search", json.RawMessage(`{"query":"x"}`)))
	if !strings.Contains(got, `"status":"error"`) {
		t.Fatalf("got %s", got)
	}
}

func TestLocalTracks(t *testing.T) {
	ctx := context.Background()
	reg, _ := stubRegistry(t, "")
	src := LocalTracks(NewLocalTools(reg, nil))
	if src.Name() != "local" {
		t.Fatal(src.Name())
	}
	found, err := src.Search(ctx, "song", 5)
	if err != nil || len(found) != 1 || found[0].Title != "Song" {
		t.Fatalf("search: %v %v", err, found)
	}
	info, err := src.Lookup(ctx, "", "song", true, 0)
	if err != nil || info.ID != "abc" || info.URL != "http://stream.test/a" {
		t.Fatalf("lookup: %v %+v", err, info)
	}
	// Local first, then MCP: a transient local failure falls through.
	bad := tools.NewMusicService(nil, "")
	failing := LocalTracks(NewLocalTools(tools.NewRegistry(nil, nil, nil, bad), nil))
	chain := FallbackTracks{Primary: failing, Secondary: src}
	if info, err := chain.Lookup(ctx, "abc", "", true, 0); err != nil || info.URL == "" {
		t.Fatalf("fallback: %v %+v", err, info)
	}
}

// A tool skipped for lack of config is not local: the remote server's serves it.
func TestLocalToolsSkip(t *testing.T) {
	ctx := context.Background()
	reg, _ := stubRegistry(t, "")
	l := NewLocalTools(reg, nil, "web_search", "music_search", "music_track")
	if got := strings.Join(l.Names(), ","); got != "news_headlines,weather" {
		t.Fatalf("names %s", got)
	}
	if l.Has("web_search") || l.Has("music_track") || !l.Has("weather") {
		t.Fatal("Has")
	}
	if got := names(l.Declarations(ctx, nil, false)); got != "news_headlines,weather" {
		t.Fatalf("declarations %s", got)
	}
	remote := &fakeTools{name: "remote", have: map[string]bool{"web_search": true, "music_track": true}}
	s := ToolSet{Local: l, Remote: remote}
	if got := string(s.Call(ctx, "web_search", nil)); got != `{"result":"remote"}` {
		t.Fatalf("got %s", got)
	}
	if err := s.CallStructured(ctx, "music_track", nil, nil); err == nil || err.Error() != "remote" {
		t.Fatalf("got %v", err)
	}
}
