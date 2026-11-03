package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"nexus/config"
	"nexus/pkg/audio"
	"nexus/pkg/bluetooth"
	"nexus/pkg/coordinator"
	"nexus/pkg/resolver"
	"nexus/pkg/transport"
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
		"player", cfg.Player,
		"requireBluetooth", cfg.RequireBluetooth,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// 1. The player: mpv (it also gives the desktop MPRIS), or the native
	// WebM/Opus player for a machine without mpv.
	var player audio.AudioPlayer
	if cfg.Player == "native" {
		player = audio.NewNativePlayer(audio.AplayOutput(cfg.AudioDevice))
		slog.Info("Native player", "device", cfg.AudioDevice)
	} else {
		if cfg.AutoSpawnMPV {
			procMgr := audio.NewProcessManager(cfg.MPVPath, cfg.MPVSocket)
			if err := procMgr.EnsureRunning(ctx); err != nil {
				slog.Error("Failed to initialize mpv process", "err", err)
				os.Exit(1)
			}
			defer procMgr.Stop()
		}
		mpv := audio.NewMPVIPCClient(cfg.MPVSocket)
		if err := mpv.Connect(); err != nil {
			slog.Error("Failed to connect to mpv IPC socket", "err", err, "socket", cfg.MPVSocket)
			os.Exit(1)
		}
		player = mpv
	}
	defer player.Close()

	// 2. Stream Resolver
	res, err := resolver.NewResolver(cfg.ResolverType, cfg.ResolverURL)
	if err != nil {
		slog.Error("Failed to initialize stream resolver", "err", err)
		os.Exit(1)
	}
	slog.Info("Stream resolver initialized", "driver", res.Name())

	// 3. Bluetooth: the pairing API, and the speaker that gates the session.
	bt := &bluetooth.Ctl{OneSpeaker: cfg.RequireBluetooth && cfg.OneSpeaker}
	var speakerMu sync.Mutex
	var speaker bluetooth.Device
	var haveSpeaker bool
	if cfg.BluetoothAPI != "" {
		api := &bluetooth.API{Ctl: bt, Active: func() (bluetooth.Device, bool) {
			speakerMu.Lock()
			defer speakerMu.Unlock()
			return speaker, haveSpeaker
		}}
		srv := &http.Server{Addr: cfg.BluetoothAPI, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			slog.Info("Bluetooth API listening", "addr", cfg.BluetoothAPI)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Bluetooth API stopped", "err", err)
			}
		}()
		defer srv.Close()
	}

	// 4. The session: transport to Nexus Core (or the hub) + coordinator.
	// Without --require-bluetooth it runs for the daemon's life; with it,
	// only while a speaker is connected, so the hub lists this satellite
	// only when it can play.
	var sessionMu sync.Mutex
	var coord *coordinator.Coordinator
	startSession := func() {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		var trans transport.Transport
		if !cfg.Standalone {
			wsTrans := transport.NewWebSocketTransport()
			if err := wsTrans.Connect(ctx, cfg.EndpointURL()); err != nil {
				slog.Warn("Initial transport connection scheduled", "endpoint", cfg.EndpointURL())
			}
			trans = wsTrans
		} else {
			slog.Info("Running in STANDALONE mode (Nexus Core connection disabled)")
		}
		coord = coordinator.NewCoordinator(cfg, trans, res, player)
		// A Bluetooth speaker's own volume (remembered per speaker), when
		// PipeWire is there; else the player's.
		if sv := (audio.SinkVolume{}); cfg.HubURL != "" && cfg.RequireBluetooth && sv.Available() {
			coord.SetVolumeControl(sv)
		}
		if err := coord.Start(); err != nil {
			slog.Error("Failed to start coordinator", "err", err)
			os.Exit(1)
		}
	}
	stopSession := func() {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		if coord != nil {
			coord.Stop() // stops the music and leaves the hub
			coord = nil
		}
	}
	defer func() { stopSession() }()

	baseName := cfg.SatelliteName
	if cfg.RequireBluetooth {
		// The output failing (aplay exits when the speaker goes) checks the
		// speaker at once; if it is gone, the session ends without telling
		// the hub "error" (which would skip to the next song).
		poke := make(chan struct{}, 1)
		player = &btGatedPlayer{AudioPlayer: player, gone: func() bool {
			select {
			case poke <- struct{}{}:
			default:
			}
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, ok, err := bt.ConnectedSink(pctx)
			return err == nil && !ok
		}}
		changes := make(chan bluetooth.Device, 4)
		go bt.Watch(ctx, 3*time.Second, poke, func(d bluetooth.Device, ok bool) {
			speakerMu.Lock()
			speaker, haveSpeaker = d, ok
			speakerMu.Unlock()
			if !ok {
				d = bluetooth.Device{}
			}
			changes <- d
		})
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-changes:
					stopSession()
					if d.MAC == "" {
						slog.Info("No Bluetooth speaker: not offering playback")
						continue
					}
					cfg.SatelliteName = baseName
					if cfg.NameFromBluetooth && d.Name != "" {
						cfg.SatelliteName = d.Name
					}
					slog.Info("Bluetooth speaker connected: offering playback", "speaker", d.Name, "as", cfg.SatelliteName)
					startSession()
				}
			}
		}()
		slog.Info("Waiting for a Bluetooth speaker before joining")
	} else {
		startSession()
	}

	// 5. Test playback if requested via CLI
	if cfg.TestVideoID != "" && coord != nil {
		slog.Info("CLI test playback requested", "videoId", cfg.TestVideoID)
		c := coord
		go func() {
			time.Sleep(500 * time.Millisecond) // brief wait for mpv readiness
			if err := c.PlayTrack(ctx, cfg.TestVideoID, 0); err != nil {
				slog.Error("Test playback failed", "err", err)
			}
		}()
	}

	slog.Info("🛰️ Nexus Orbit Daemon is running. Press Ctrl+C to terminate.")

	sig := <-sigCh
	slog.Info("Shutdown signal received, shutting down gracefully...", "signal", sig)
	cancel()
}

// btGatedPlayer drops an output error when the Bluetooth speaker is gone.
type btGatedPlayer struct {
	audio.AudioPlayer
	gone func() bool
}

func (p *btGatedPlayer) OnTrackEnded(fn func(err error)) {
	p.AudioPlayer.OnTrackEnded(func(err error) {
		if err != nil && errors.Is(err, audio.ErrOutput) && p.gone() {
			slog.Info("Speaker gone mid-song; leaving without an error", "err", err)
			return
		}
		fn(err)
	})
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
