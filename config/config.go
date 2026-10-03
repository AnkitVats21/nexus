package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds runtime configuration for the nexus-orbit daemon.
type Config struct {
	NexusHost     string
	NexusPort     int
	WSPath        string
	SatelliteName string
	SatelliteID   string
	ResolverType  string
	ResolverURL   string
	MPVPath       string
	MPVSocket     string
	AutoSpawnMPV  bool
	Standalone    bool
	TestVideoID   string
	LogLevel      string
	// The hub (nexus-hub) to join as an edge ("" = talk to the board's
	// /api/orbit/ws), and its token (the first line of the token file).
	HubURL   string
	HubToken string
}

// LoadConfig parses flags, environment variables, and sensible defaults.
func LoadConfig() (*Config, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "orbit-satellite"
	}

	defaultSocket := defaultSocketPath()

	cfg := &Config{}

	var portFlag int
	flag.StringVar(&cfg.NexusHost, "nexus-host", getEnv("NEXUS_HOST", "nexus.local"), "Hostname or IP of Nexus Core")
	flag.IntVar(&portFlag, "nexus-port", 0, "HTTP/WS port on Nexus Core (defaults to 8080 for localhost/stub, 80 for remote board)")
	flag.StringVar(&cfg.WSPath, "ws-path", getEnv("NEXUS_WS_PATH", "/api/orbit/ws"), "WebSocket path on Nexus Core")
	flag.StringVar(&cfg.SatelliteName, "satellite-name", getEnv("SATELLITE_NAME", hostname), "Human-readable satellite name")
	flag.StringVar(&cfg.SatelliteID, "satellite-id", getEnv("SATELLITE_ID", fmt.Sprintf("satellite-%s", hostname)), "Unique satellite identifier")
	flag.StringVar(&cfg.ResolverType, "resolver", getEnv("RESOLVER_TYPE", "auto"), "Stream resolver: auto, direct, invidious")
	flag.StringVar(&cfg.ResolverURL, "resolver-url", getEnv("RESOLVER_URL", "https://stream.ankitm.xyz"), "Invidious daemon resolver endpoint")
	flag.StringVar(&cfg.MPVPath, "mpv-path", getEnv("MPV_PATH", "/usr/bin/mpv"), "Path to mpv binary")
	flag.StringVar(&cfg.MPVSocket, "mpv-socket", getEnv("MPV_SOCKET", defaultSocket), "Unix domain socket for mpv IPC")
	flag.BoolVar(&cfg.AutoSpawnMPV, "auto-spawn-mpv", getEnvBool("AUTO_SPAWN_MPV", true), "Automatically spawn headless mpv if not running")
	flag.BoolVar(&cfg.Standalone, "standalone", getEnvBool("STANDALONE", false), "Run in standalone mode (no Nexus connection)")
	flag.StringVar(&cfg.TestVideoID, "play", "", "Play a specific YouTube videoId immediately on launch (for testing)")
	flag.StringVar(&cfg.LogLevel, "log-level", getEnv("LOG_LEVEL", "info"), "Logging level (debug, info, warn, error)")
	flag.StringVar(&cfg.HubURL, "hub", getEnv("NEXUS_HUB", ""), "Join a nexus-hub as an edge, e.g. ws://host:8380/hub/edge (default $NEXUS_HUB; empty = the board's /api/orbit/ws)")
	home, _ := os.UserHomeDir()
	hubTokenFile := flag.String("hub-token-file", filepath.Join(home, ".config", "nexus", "hub_tokens"), "Hub token (first line)")

	flag.Parse()

	if cfg.HubURL != "" {
		data, err := os.ReadFile(*hubTokenFile)
		if err != nil {
			return nil, fmt.Errorf("hub token: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				cfg.HubToken = line
				break
			}
		}
	}

	if portFlag != 0 {
		cfg.NexusPort = portFlag
	} else if envPort := getEnvInt("NEXUS_PORT", 0); envPort != 0 {
		cfg.NexusPort = envPort
	} else if cfg.NexusHost == "localhost" || cfg.NexusHost == "127.0.0.1" || cfg.NexusHost == "::1" {
		cfg.NexusPort = 8080
	} else {
		cfg.NexusPort = 80
	}

	return cfg, nil
}

// EndpointURL formats the full WebSocket URL for connecting to Nexus Core.
func (c *Config) EndpointURL() string {
	if c.HubURL != "" {
		return c.HubURL
	}
	return fmt.Sprintf("ws://%s:%d%s", c.NexusHost, c.NexusPort, c.WSPath)
}

func defaultSocketPath() string {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir != "" {
		return filepath.Join(runtimeDir, "nexus-orbit.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("nexus-orbit-%d.sock", os.Getuid()))
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.ParseBool(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}
