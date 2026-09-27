# Nexus Orbit (Satellite Client) — Architecture & Design Specification

**Project:** `nexus-orbit`  
**Role:** Host-side Satellite Daemon & Co-Player for the Waveshare ESP32-S3 ("Nexus Core")  
**Target Platform:** Linux / PC, Raspberry Pi / SBC (Extendable to macOS / Windows)  
**Implementation Language:** Go  

---

## 1. Overview & Goals

`nexus-orbit` is a lightweight, background daemon that turns your PC or Raspberry Pi into a high-fidelity **Satellite Co-Player** within the Nexus ecosystem.

When Nexus Core routes audio playback to this Satellite:
1. Nexus forwards the active `videoId`, track metadata, and `position_ms`.
2. The Satellite resolves the playable audio stream via its **Pluggable Resolver** (direct local `mpv`/`yt-dlp` on fast PCs, or remote `invidious-daemon` on low-power SBCs like Raspberry Pi).
3. The Satellite renders audio via a local hardware-accelerated audio pipeline (`mpv` JSON IPC).
4. Telemetry (position, playback state) is continuously reported back to Nexus Core.
5. Desktop controls (Linux MPRIS, hardware media keys) integrate seamlessly so physical keyboard keys control playback across the entire system.

### Key Design Tenets
* **Firmware-Decoupled Transport:** Because firmware development on Waveshare is evolving independently (and current firmware has a single-client limitation on `/api/ws`), the transport layer is abstracted. It can connect to `/api/orbit/ws`, `/api/ws`, or run in standalone test mode.
* **Pluggable Resolver:** PC users can resolve YouTube streams directly through `mpv`'s local `yt-dlp` integration with zero external infrastructure dependencies. Raspberry Pi users can resolve via `stream.ankitm.xyz` (`invidious-daemon`) to avoid 100% CPU spikes during Python extraction.
* **Lean Client Architecture:** Unlike legacy relay prototypes, `nexus-orbit` has no local SQLite database, no local queue authority, no MQTT broker requirement, and no disk recording. Nexus Core is the authoritative queue and state master.

---

## 2. System Architecture

```
                                  +------------------------------+
                                  |    invidious-daemon Host     |
                                  | (https://stream.ankitm.xyz)  |
                                  +--------------+---------------+
                                                 ^
                         (Raspberry Pi mode)     | 2b. Remote Stream URL
                         Resolve Stream URL      |
+-----------------------+         +--------------+---------------+
|   Nexus Core (ESP32)  |         |         nexus-orbit          |
|   (Control & Master)  |         |      (Satellite Daemon)      |
+-----------+-----------+         +--------------+---------------+
            ^                                    |
            | 1. Control / Routing Events        | 2a. Direct URL / IPC
            |    (Dedicated WS / Configurable)   | (PC Mode via yt-dlp)
            v                                    v
+-----------+-----------+                 +--------------+
| Transport & Dispatch  |                 |  mpv Engine  |---> [ Soundcard / DAC ]
+-----------------------+                 | (Audio Sink) |
            ^                             +-------+------+
            |                                    ^
+-----------+-----------+                 +------+-------+
|  State Coordinator    |<--------------->| Linux MPRIS  |<--- [ Keyboard Media Keys ]
|   (Local State Mach)  |                 | (D-Bus API)  |
+-----------------------+                 +--------------+
```

---

## 3. Core Component Breakdown

### 3.1. Transport Adapter (`pkg/transport`)
* **Responsibility:** Manages the bidirectional communication link with Nexus Core.
* **Interface:**
  ```go
  type Transport interface {
      Connect(ctx context.Context, endpoint string) error
      Send(msg []byte) error
      Receive() (<-chan []byte, <-chan error)
      Close() error
  }
  ```
* **Features:**
  * Configurable endpoint (e.g., `ws://nexus.local/api/orbit/ws` or `ws://nexus.local/api/ws`).
  * Non-blocking reconnection loop with exponential backoff (1s -> 2s -> 4s -> max 30s) + jitter.
  * Automatic ping/pong liveness checks and graceful shutdown.

### 3.2. Protocol & Message Dispatcher (`pkg/protocol`)
* **Responsibility:** Serializes/deserializes command envelopes exchanged with Nexus Core.
* **Inbound Commands (from Nexus Core):**
  * `orbit_route`: Directs active playback to this Satellite with `videoId`, `title`, `artist`, `duration`, and `position_ms`.
  * `orbit_play`: Start playing a specific track.
  * `orbit_pause`: Pause playback.
  * `orbit_resume`: Resume playback.
  * `orbit_seek`: Seek to a specific `position_ms`.
  * `orbit_volume`: Set volume percentage (0–100, applied to `mpv` internal volume).
  * `orbit_stop`: Stop audio output and flush player buffer.
* **Outbound Events (to Nexus Core):**
  * `satellite_register`: Announces satellite identity, hostname, and capabilities upon connect.
  * `orbit_report`: Emits playback telemetry (`position_ms`, `state`, `volume`) periodically (every 1 second).
  * `orbit_event`: Reports lifecycle events (`track_ended`, `buffering`, `error`).
  * `orbit_action`: Forwards local user intent from MPRIS media keys (e.g. `play`, `pause`, `next`, `prev`).

### 3.3. Pluggable Stream Resolver (`pkg/resolver`)
* **Responsibility:** Resolves YouTube `videoId` into a playable URL and track metadata.
* **Interface:**
  ```go
  type ResolvedTrack struct {
      PlaybackURL  string // Direct HTTP audio stream or YouTube URL
      Title        string
      Artist       string
      DurationSecs int
  }

  type Resolver interface {
      Resolve(ctx context.Context, videoID string) (*ResolvedTrack, error)
      Name() string
  }
  ```
* **Drivers:**
  1. **`direct` (PC Mode):** Passes `https://www.youtube.com/watch?v={videoId}` to `mpv`, letting `mpv`'s native `yt-dlp` hook resolve formats locally. Fast, reliable on PCs, and independent of external servers.
  2. **`invidious` (Raspberry Pi / SBC Mode):** Calls `GET https://stream.ankitm.xyz/api/v1/videos/{videoId}`, extracting the highest quality Opus/AAC stream URL. Fast (~150ms HTTP JSON) and uses 0% client CPU.
  3. **`auto` (Hybrid Fallback):** Tries primary driver, falls back to the secondary if resolution fails.

### 3.4. Audio Engine (`pkg/audio`)
* **Responsibility:** High-fidelity, reliable audio output on the host PC/SBC.
* **Engine Choice:** Headless `mpv` controlled via Unix Domain Socket JSON-IPC.
* **Process Lifecycle:**
  * Orbit automatically spawns and manages a headless `mpv` process (`--idle --no-video --input-ipc-server=<sock> --really-quiet`).
  * Socket path defaults to `$XDG_RUNTIME_DIR/nexus-orbit.sock` with fallback to `/tmp/nexus-orbit-<uid>.sock`.
  * Allows attaching to an existing `mpv` instance via `--mpv-socket`.
  * Handles graceful termination (`SIGTERM`) on daemon exit.
* **Interface:**
  ```go
  type AudioPlayer interface {
      LoadURL(url string, startPosMs int64) error
      Pause() error
      Resume() error
      Seek(posMs int64) error
      SetVolume(vol int) error
      Stop() error
      GetStatus() (PlayerStatus, error)
      Close() error
  }
  ```

### 3.5. Desktop Integration via Native mpv MPRIS
* **Responsibility:** Seamless desktop OS integration (Linux MPRIS & media keys).
* **Native mpv MPRIS:**
  * Headless `mpv` automatically exports `org.mpris.MediaPlayer2.mpv` on the user session D-Bus.
  * When playing a track, `mpv` extracts and displays the title, artist, duration, and thumbnail (`mpris:artUrl`) to desktop widgets (GNOME, KDE, Waybar).
  * Hardware media keys (Play/Pause, Seek, Volume) control `mpv` directly.
  * `nexus-orbit` observes these property changes via its Unix domain socket JSON-IPC and immediately synchronizes playback state back to Nexus Core.
  * Eliminates redundant D-Bus code and prevents duplicate player widgets on the desktop.

### 3.6. State Coordinator (`pkg/coordinator`)
* **Responsibility:** Central controller orchestrating Transport, Resolver, and Audio Engine.
* **Authority Model:**
  * **Nexus Core is Queue Authority:** All playlist progression, shuffle, repeat, and track sequencing remain on Nexus.
  * **Reactive Local Synchronization:** Whenever local `mpv` state changes (e.g. user toggles play/pause via keyboard media keys), the coordinator updates local state and transmits telemetry back to Nexus Core.
* **State Machine:**
  * `IDLE`: Connected, waiting for audio routing.
  * `RESOLVING`: Received `videoId`, resolving stream URL via selected driver.
  * `BUFFERING`: Stream URL loaded into `mpv`, filling audio buffer.
  * `PLAYING`: Actively rendering audio.
  * `PAUSED`: Playback suspended.
  * `ERROR`: Stream error or resolver failure (reported back to Nexus).

---

## 4. Project Structure

```
nexus-orbit/
├── cmd/
│   └── orbit/
│       └── main.go              # CLI entry point, flag parsing & signal handling
├── pkg/
│   ├── audio/                   # mpv process manager & Unix socket IPC client
│   │   ├── player.go            # AudioPlayer interface & data types
│   │   ├── mpv_ipc.go           # Low-level JSON-IPC client
│   │   └── mpv_process.go       # Managed mpv subprocess runner
│   ├── coordinator/             # State machine & lifecycle management
│   │   └── coordinator.go
│   ├── protocol/                # JSON message models & serialization
│   │   ├── messages.go          # Inbound / outbound protocol envelopes
│   │   └── events.go            # Event types
│   ├── resolver/                # Pluggable stream resolver
│   │   ├── resolver.go          # Resolver interface & factory
│   │   ├── direct.go            # Direct PC / local yt-dlp driver
│   │   └── invidious.go         # Remote invidious-daemon HTTP driver
│   └── transport/               # Transport abstraction (WebSocket client)
│       ├── transport.go         # Transport interface
│       └── websocket.go         # WebSocket client implementation
├── config/
│   └── config.go                # Config parser (flags / env vars)
├── go.mod
├── go.sum
└── DESIGN.md                    # This document
```

---

## 5. Configuration & CLI Parameters

| Flag | Env Var | Options | Default | Description |
|---|---|---|---|---|
| `--nexus-host` | `NEXUS_HOST` | string | `nexus.local` | Hostname or IP of Waveshare ESP32 board |
| `--nexus-port` | `NEXUS_PORT` | int | `80` | Port of the Waveshare HTTP/WS server |
| `--ws-path` | `NEXUS_WS_PATH` | string | `/api/orbit/ws` | WebSocket endpoint path |
| `--resolver` | `RESOLVER_TYPE` | `auto`, `direct`, `invidious` | `auto` | Stream resolver strategy |
| `--resolver-url` | `RESOLVER_URL` | URL string | `https://stream.ankitm.xyz` | Endpoint for `invidious` mode |
| `--satellite-name`| `SATELLITE_NAME`| string | `OS Hostname` | Human-readable satellite name |
| `--satellite-id` | `SATELLITE_ID` | string | Auto-generated UUID | Persistent machine identifier |
| `--mpv-path` | `MPV_PATH` | string | `/usr/bin/mpv` | Path to the mpv binary |
| `--mpv-socket` | `MPV_SOCKET` | path | `""` | Optional external socket to attach to |
| `--standalone` | `STANDALONE` | bool | `false` | Run without connecting to Nexus (test mode) |

---

## 6. Implementation Plan

1. **Step 1 — Foundation:** Initialize `go.mod`, implement `config/` and `pkg/protocol/`.
2. **Step 2 — Pluggable Resolver (`pkg/resolver/`):** Implement `direct.go` and `invidious.go` with unit/standalone tests.
3. **Step 3 — Audio Engine (`pkg/audio/`):** Implement `mpv_ipc.go` (modernizing the clean client logic) and `mpv_process.go`.
4. **Step 4 — Transport (`pkg/transport/`):** Implement robust WebSocket client with auto-reconnect and standalone testability.
5. **Step 5 — State Coordinator (`pkg/coordinator/`):** Connect Transport, Resolver, and Audio Engine with state reporting.
6. **Step 6 — Linux MPRIS Integration (`pkg/desktop/`):** Implement D-Bus MPRIS controls and media key bindings.
7. **Step 7 — CLI & Verification:** Assemble `cmd/orbit/main.go`, verify standalone playback with a test YouTube track, and test end-to-end.
