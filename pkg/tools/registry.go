package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The registry is the one list of tools. The MCP server registers them from
// it (Registry.Register) and an in-process caller (nexus-hub) lists and calls
// them from it (Tools, Call), so the two cannot drift apart.

// Config is everything the tools need from the outside. The package reads no
// environment: the caller does and passes the values in.
type Config struct {
	// HTTPClient serves news, weather and search; nil = a client per tool
	// with its own default timeout.
	HTTPClient *http.Client
	// Gemini API key for web_search: the key itself, or a file holding it
	// (read at call time). Neither set: web_search answers with an error.
	GeminiAPIKey  string
	GeminiKeyFile string
	// MusicURL is the Invidious-compatible API for music_search and
	// music_track; empty: those tools answer with an error.
	MusicURL string
	// MusicClient serves the music API; nil = 25 s timeout (resolving a
	// stream runs yt-dlp behind the API: 10 s measured cold).
	MusicClient *http.Client
	// Lights are the smart bulbs on the LAN, one tool each (named after the
	// light: "work room light" is work_room_light); none = no light tool.
	Lights []LightConfig
}

// Result is a tool call's outcome, as the MCP server returns it.
type Result struct {
	IsError bool
	// Text is the tool's text content, joined with newlines (the voice
	// readout, or the error message).
	Text string
	// Structured is the structured result as JSON; nil when there is none.
	Structured json.RawMessage
}

// Tool is one tool: its declaration and how to call it.
type Tool struct {
	Name        string
	Description string
	// DeviceOnly: the device calls it itself, the voice model is not
	// offered it (DeviceOnlyMeta).
	DeviceOnly bool
	// InputSchema is the JSON schema of the arguments.
	InputSchema json.RawMessage

	call     func(ctx context.Context, args json.RawMessage) (Result, error)
	register func(s *mcp.Server)
}

// Call runs the tool with JSON arguments (nil or "null" = none). The error is
// for arguments that do not decode; a tool's own failure is a Result with
// IsError set. Error text never carries the API keys.
func (t *Tool) Call(ctx context.Context, args json.RawMessage) (Result, error) {
	return t.call(ctx, args)
}

// Registry is the ordered set of tools.
type Registry struct {
	tools  []*Tool
	byName map[string]*Tool
}

// Tools returns the tools in registration order.
func (r *Registry) Tools() []*Tool { return r.tools }

// Get returns the tool of that name.
func (r *Registry) Get(name string) (*Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Register adds every tool to an MCP server.
func (r *Registry) Register(s *mcp.Server) {
	for _, t := range r.tools {
		t.register(s)
	}
}

func (r *Registry) add(t *Tool) {
	if r.byName == nil {
		r.byName = map[string]*Tool{}
	}
	r.tools = append(r.tools, t)
	r.byName[t.Name] = t
}

// NewRegistry builds the registry from services. A nil service gets a default
// one (no keys, no music API).
func NewRegistry(news *NewsService, weather *WeatherService, search *SearchService, music *MusicService) *Registry {
	if news == nil {
		news = NewNewsService(nil)
	}
	if weather == nil {
		weather = NewWeatherService(nil)
	}
	if search == nil {
		search = NewSearchService(nil, "", "")
	}
	if music == nil {
		music = NewMusicService(nil, "")
	}
	r := &Registry{}
	addTool(r, "news_headlines", "Get top news headlines or search by topic/query for voice readout", false, news.Handler())
	addTool(r, "web_search", "Search the web using Gemini with Google Search grounding for real-time information", false, search.Handler())
	addTool(r, "weather", "Get current weather conditions and 1-7 day forecast for any location", false, weather.Handler())
	// Device-only tools: the device calls these itself; it does not offer
	// them to the voice model.
	addTool(r, "music_search", "Search YouTube for songs (device use)", true, music.SearchHandler())
	addTool(r, "music_track", "A song's details, audio stream URL and related songs, by video id or search query (device use)", true, music.TrackHandler())
	return r
}

// NewDefaultRegistry builds the services from cfg and the registry from them.
func NewDefaultRegistry(cfg Config) *Registry {
	musicClient := cfg.MusicClient
	if musicClient == nil {
		musicClient = &http.Client{Timeout: 25 * time.Second}
	}
	r := NewRegistry(
		NewNewsService(cfg.HTTPClient),
		NewWeatherService(cfg.HTTPClient),
		NewSearchService(cfg.HTTPClient, cfg.GeminiAPIKey, cfg.GeminiKeyFile),
		NewMusicService(musicClient, cfg.MusicURL),
	)
	for _, l := range cfg.Lights {
		r.AddLight(l)
	}
	return r
}

// AddLight adds the tool for one smart bulb.
func (r *Registry) AddLight(cfg LightConfig) {
	s := NewLightService(cfg)
	addTool(r, LightToolName(s.cfg.Name),
		fmt.Sprintf("Control the %s: turn it on, off or toggle, set brightness, warm/cool white, or a colour. With no arguments, say whether it is on and how it is set.", s.cfg.Name),
		false, s.Handler())
}

// LightToolName is the tool name for a light's name: "Work room light" is
// work_room_light.
func LightToolName(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), "_")
}

type handlerFunc[P any] func(context.Context, *mcp.CallToolRequest, *P) (*mcp.CallToolResult, any, error)

func addTool[P any](r *Registry, name, desc string, deviceOnly bool, h handlerFunc[P]) {
	schema, err := jsonschema.For[P](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: schema: %v", name, err))
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("tool %s: schema: %v", name, err))
	}
	mt := &mcp.Tool{Name: name, Description: desc}
	if deviceOnly {
		mt.Meta = DeviceOnlyMeta
	}
	r.add(&Tool{
		Name: name, Description: desc, DeviceOnly: deviceOnly, InputSchema: raw,
		register: func(s *mcp.Server) { mcp.AddTool[*P, any](s, mt, mcp.ToolHandlerFor[*P, any](h)) },
		call: func(ctx context.Context, args json.RawMessage) (Result, error) {
			var p P
			if s := strings.TrimSpace(string(args)); s != "" && s != "null" {
				if err := json.Unmarshal(args, &p); err != nil {
					return Result{}, fmt.Errorf("%s: invalid arguments: %w", name, err)
				}
			}
			res, out, err := h(ctx, nil, &p)
			if err != nil {
				return Result{IsError: true, Text: err.Error()}, nil
			}
			var texts []string
			if res != nil {
				for _, c := range res.Content {
					if tc, ok := c.(*mcp.TextContent); ok && tc.Text != "" {
						texts = append(texts, tc.Text)
					}
				}
			}
			r := Result{IsError: res != nil && res.IsError, Text: strings.Join(texts, "\n")}
			if out != nil {
				if b, err := json.Marshal(out); err == nil && string(b) != "null" {
					r.Structured = b
				}
			}
			return r, nil
		},
	})
}
