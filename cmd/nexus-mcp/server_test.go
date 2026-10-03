package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nexus/pkg/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAuthMiddleware(t *testing.T) {
	const validToken = "secret-test-token-12345"
	dummyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authorized"))
	})

	handler := AuthMiddleware(validToken, dummyHandler)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"No header", "", http.StatusUnauthorized},
		{"Basic auth", "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		{"Wrong token", "Bearer invalid-token", http.StatusUnauthorized},
		{"Valid token", "Bearer " + validToken, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

type authTransport struct {
	token string
	base  http.RoundTripper
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+a.token)
	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

func TestInProcessMCPClient(t *testing.T) {
	// 1. Stubbed weather backend (no external network)
	stubWeatherServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/search") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{
				"results": [{
					"name": "London",
					"latitude": 51.5085,
					"longitude": -0.1257,
					"country": "United Kingdom",
					"timezone": "Europe/London"
				}]
			}`)
			return
		}
		if strings.Contains(r.URL.Path, "/forecast") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{
				"timezone": "Europe/London",
				"current": {
					"temperature_2m": 19.0,
					"relative_humidity_2m": 70,
					"apparent_temperature": 18.5,
					"precipitation": 0.0,
					"weather_code": 0,
					"wind_speed_10m": 10.0
				},
				"daily": {
					"time": ["2026-09-29"],
					"weather_code": [0],
					"temperature_2m_max": [22.0],
					"temperature_2m_min": [14.0],
					"precipitation_probability_max": [10],
					"sunrise": ["2026-09-29T06:52"],
					"sunset": ["2026-09-29T18:42"]
				}
			}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer stubWeatherServer.Close()

	weatherSvc := tools.NewWeatherService(stubWeatherServer.Client())
	weatherSvc.GeocodingBase = stubWeatherServer.URL + "/search"
	weatherSvc.ForecastBase = stubWeatherServer.URL + "/forecast"

	// 2. Start MCP server over HTTP
	const testToken = "test-token-xyz"
	httpHandler, _, err := NewHTTPHandler(ServerConfig{
		Token:          testToken,
		WeatherService: weatherSvc,
	})
	if err != nil {
		t.Fatalf("failed to create HTTP handler: %v", err)
	}

	mcpHTTPServer := httptest.NewServer(httpHandler)
	defer mcpHTTPServer.Close()

	// 3. Connect in-process MCP Client using official SDK
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "in-process-test-client",
		Version: "1.0.0",
	}, nil)

	authedClient := &http.Client{
		Transport: &authTransport{
			token: testToken,
			base:  mcpHTTPServer.Client().Transport,
		},
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:             mcpHTTPServer.URL + "/mcp",
		HTTPClient:           authedClient,
		DisableStandaloneSSE: true,
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client failed to connect: %v", err)
	}
	defer session.Close()

	// 4. List tools
	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	toolNames := make(map[string]bool)
	for _, tool := range toolsResult.Tools {
		toolNames[tool.Name] = true
	}

	expectedTools := []string{"news_headlines", "web_search", "weather", "music_search", "music_track"}
	for _, name := range expectedTools {
		if !toolNames[name] {
			t.Errorf("expected tool %q not found in tools/list", name)
		}
	}

	// Only the device's own tools carry the marker that keeps them away
	// from the voice model; a missing marker would put them in its prompt.
	deviceOnly := map[string]bool{"music_search": true, "music_track": true}
	for _, tool := range toolsResult.Tools {
		marked := tool.Meta["nexus/device_only"] == true
		if marked != deviceOnly[tool.Name] {
			t.Errorf("tool %q: device_only marker %v, want %v", tool.Name, marked, deviceOnly[tool.Name])
		}
	}

	// 5. Call weather tool
	callRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "weather",
		Arguments: map[string]any{
			"location": "London",
			"days":     1,
		},
	})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	if callRes.IsError {
		t.Fatalf("weather call returned tool error: %+v", callRes)
	}

	foundLondon := false
	for _, c := range callRes.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if strings.Contains(tc.Text, "London") {
				foundLondon = true
				break
			}
		}
	}

	if !foundLondon {
		t.Errorf("weather tool result did not contain expected text about London: %+v", callRes.Content)
	}
}
