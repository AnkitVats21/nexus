package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
}

// LoadConfig parses flags, environment variables, and sensible defaults.
func LoadConfig() (*Config, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "orbit-satellite"
	}

	defaultSocket := defaultSocketPath()

	cfg := &Config{}

	flag.StringVar(&cfg.NexusHost, "nexus-host", getEnv("NEXUS_HOST", "nexus.local"), "Hostname or IP of Nexus Core")
	flag.IntVar(&cfg.NexusPort, "nexus-port", getEnvInt("NEXUS_PORT", 80), "HTTP/WS port on Nexus Core")
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

	flag.Parse()

	return cfg, nil
}

// EndpointURL formats the full WebSocket URL for connecting to Nexus Core.
func (c *Config) EndpointURL() string {
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
