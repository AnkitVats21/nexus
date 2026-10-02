package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nexus-mcp/tools"
)

func main() {
	token := strings.TrimSpace(os.Getenv("NEXUS_MCP_TOKEN"))
	if token == "" {
		fmt.Fprintln(os.Stderr, "Error: NEXUS_MCP_TOKEN environment variable is required")
		os.Exit(1)
	}

	addr := strings.TrimSpace(os.Getenv("NEXUS_MCP_ADDR"))
	if addr == "" {
		addr = "0.0.0.0:8787"
	}

	apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	keyFile := strings.TrimSpace(os.Getenv("GEMINI_API_KEY_FILE"))

	// Create shared HTTP client with 8-second timeout for outbound requests
	httpClient := &http.Client{Timeout: 8 * time.Second}

	newsSvc := tools.NewNewsService(httpClient)
	weatherSvc := tools.NewWeatherService(httpClient)
	searchSvc := tools.NewSearchService(httpClient, apiKey, keyFile)

	// Invidious-compatible API for the device's music lookups.
	musicURL := strings.TrimSpace(os.Getenv("NEXUS_MCP_MUSIC_API"))
	if musicURL == "" {
		musicURL = "https://stream.ankitm.xyz"
	}
	// Resolving a stream runs yt-dlp behind the API: 10 s measured cold.
	musicSvc := tools.NewMusicService(&http.Client{Timeout: 25 * time.Second}, musicURL)

	cfg := ServerConfig{
		Token:          token,
		Addr:           addr,
		NewsService:    newsSvc,
		WeatherService: weatherSvc,
		SearchService:  searchSvc,
		MusicService:   musicSvc,
	}

	handler, _, err := NewHTTPHandler(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize MCP server: %v", err)
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("nexus-mcp server listening on http://%s/mcp", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Fatalf("Server error: %v", err)
	case sig := <-sigChan:
		log.Printf("Received signal %v, shutting down gracefully...", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}
}
