package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nexus-orbit/config"
	"nexus-orbit/pkg/audio"
	"nexus-orbit/pkg/coordinator"
	"nexus-orbit/pkg/desktop"
	"nexus-orbit/pkg/resolver"
	"nexus-orbit/pkg/transport"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	setupLogging(cfg.LogLevel)

	slog.Info("🪐 Nexus Orbit Satellite Daemon initializing...",
		"satelliteID", cfg.SatelliteID,
		"satelliteName", cfg.SatelliteName,
		"resolver", cfg.ResolverType,
		"standalone", cfg.Standalone,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// 1. Audio Engine & mpv Process
	var procMgr *audio.ProcessManager
	if cfg.AutoSpawnMPV {
		procMgr = audio.NewProcessManager(cfg.MPVPath, cfg.MPVSocket)
		if err := procMgr.EnsureRunning(ctx); err != nil {
			slog.Error("Failed to initialize mpv process", "err", err)
			os.Exit(1)
		}
		defer procMgr.Stop()
	}

	playerClient := audio.NewMPVIPCClient(cfg.MPVSocket)
	if err := playerClient.Connect(); err != nil {
		slog.Error("Failed to connect to mpv IPC socket", "err", err, "socket", cfg.MPVSocket)
		os.Exit(1)
	}
	defer playerClient.Close()

	// 2. Stream Resolver
	res, err := resolver.NewResolver(cfg.ResolverType, cfg.ResolverURL)
	if err != nil {
		slog.Error("Failed to initialize stream resolver", "err", err)
		os.Exit(1)
	}
	slog.Info("Stream resolver initialized", "driver", res.Name())

	// 3. Desktop MPRIS (Linux)
	mprisMgr, err := desktop.NewMPRISManager()
	if err != nil {
		slog.Warn("Desktop MPRIS integration unavailable (continuing without D-Bus)", "err", err)
	} else {
		defer mprisMgr.Close()
	}

	// 4. Transport Link to Nexus Core
	var trans transport.Transport
	if !cfg.Standalone {
		wsTrans := transport.NewWebSocketTransport()
		if err := wsTrans.Connect(ctx, cfg.EndpointURL()); err != nil {
			slog.Warn("Initial transport connection scheduled", "endpoint", cfg.EndpointURL())
		}
		trans = wsTrans
		defer trans.Close()
	} else {
		slog.Info("Running in STANDALONE mode (Nexus Core connection disabled)")
	}

	// 5. State Coordinator
	coord := coordinator.NewCoordinator(cfg, trans, res, playerClient, mprisMgr)
	if err := coord.Start(); err != nil {
		slog.Error("Failed to start coordinator", "err", err)
		os.Exit(1)
	}
	defer coord.Stop()

	// 6. Test playback if requested via CLI
	if cfg.TestVideoID != "" {
		slog.Info("CLI test playback requested", "videoId", cfg.TestVideoID)
		go func() {
			time.Sleep(500 * time.Millisecond) // brief wait for mpv readiness
			if err := coord.PlayTrack(ctx, cfg.TestVideoID, 0); err != nil {
				slog.Error("Test playback failed", "err", err)
			}
		}()
	}

	slog.Info("🛰️ Nexus Orbit Daemon is running. Press Ctrl+C to terminate.")

	sig := <-sigCh
	slog.Info("Shutdown signal received, shutting down gracefully...", "signal", sig)
}

func setupLogging(levelStr string) {
	var lvl slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
	}
	handler := slog.NewTextHandler(os.Stdout, opts)
	slog.SetDefault(slog.New(handler))
}
