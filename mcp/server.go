package main

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"nexus-mcp/tools"
)

// ServerConfig configures the MCP server and HTTP transport.
type ServerConfig struct {
	Token          string
	Addr           string
	NewsService    *tools.NewsService
	WeatherService *tools.WeatherService
	SearchService  *tools.SearchService
}

// AuthMiddleware enforces Bearer token authentication using constant-time comparison.
func AuthMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "Unauthorized: missing Bearer token", http.StatusUnauthorized)
			return
		}
		provided := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BuildMCPServer creates and configures the MCP server instance with all tools registered.
func BuildMCPServer(cfg ServerConfig) (*mcp.Server, error) {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "nexus-mcp",
		Version: "1.0.0",
	}, nil)

	newsSvc := cfg.NewsService
	if newsSvc == nil {
		newsSvc = tools.NewNewsService(nil)
	}

	weatherSvc := cfg.WeatherService
	if weatherSvc == nil {
		weatherSvc = tools.NewWeatherService(nil)
	}

	searchSvc := cfg.SearchService
	if searchSvc == nil {
		searchSvc = tools.NewSearchService(nil, "", "")
	}

	// 1. news_headlines
	mcp.AddTool(server, &mcp.Tool{
		Name:        "news_headlines",
		Description: "Get top news headlines or search by topic/query for voice readout",
	}, newsSvc.Handler())

	// 2. web_search
	mcp.AddTool(server, &mcp.Tool{
		Name:        "web_search",
		Description: "Search the web using Gemini with Google Search grounding for real-time information",
	}, searchSvc.Handler())

	// 3. weather
	mcp.AddTool(server, &mcp.Tool{
		Name:        "weather",
		Description: "Get current weather conditions and 1-7 day forecast for any location",
	}, weatherSvc.Handler())

	return server, nil
}

// NewHTTPHandler creates the HTTP handler with streamable MCP transport and auth middleware.
func NewHTTPHandler(cfg ServerConfig) (http.Handler, *mcp.Server, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, nil, fmt.Errorf("auth token is required")
	}

	mcpServer, err := BuildMCPServer(cfg)
	if err != nil {
		return nil, nil, err
	}

	streamableHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return mcpServer
	}, &mcp.StreamableHTTPOptions{
		JSONResponse: true, // Prefer application/json responses over text/event-stream
	})

	mux := http.NewServeMux()

	// MCP endpoint on /mcp with Bearer token authentication
	authedMCP := AuthMiddleware(cfg.Token, streamableHandler)
	mux.Handle("/mcp", authedMCP)
	mux.Handle("/mcp/", authedMCP)

	// Lightweight health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK\n"))
	})

	return mux, mcpServer, nil
}
