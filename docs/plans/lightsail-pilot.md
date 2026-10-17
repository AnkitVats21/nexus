# Pilot: the hub on Lightsail

Status: plan, 2026-10-07
Builds on: `hub-multi-user.md` section 9 (where the hub runs)

## 1. Goal

Run the hub on the Lightsail server for a while and find out whether it is
good enough to be the main hub:

- Voice delay from a board at home through the server to Gemini and back,
  compared with the home hub.
- The link: drops, reconnects, the warnings we fixed on the LAN.
- Music: a server hub only resolves songs; the players fetch them straight
  from YouTube.

This is a pilot for one person: no accounts yet (M1 is built but not tested
on the device; it comes in once the pilot shows the server is worth it).

## 2. Shape

```
board (home) ──wss──┐
orbit (home) ──wss──┤   nginx (TLS, hub.<domain>) ── nexus-hub (127.0.0.1:8380) ── Gemini Live
browser ──https─────┘        basic auth on the dashboard           │
                                                                   └── resolver (invidious-daemon,
                                                                       egress from home)
players fetch songs straight from YouTube
```

- **nginx** (already on the server) ends TLS for `hub.<domain>` (certbot) and
  forwards to the hub on localhost. WebSocket upgrade headers, 1 h read
  timeouts, no buffering.
- **Edges** (`/hub/edge`, `/hub/board`) authenticate with the hub token, as
  today. A new random token for the server, not the home one.
- **Dashboard and `/api`** are behind nginx basic auth for the pilot (the hub
  has no login until M1).
- **Music**: no cache (`--cache-max-mb 0`), resolution through the
  Invidious-compatible resolver whose egress is the home IP (stream URLs are
  bound to the IP that resolved them). The hub never downloads or relays a
  song.
- **mDNS** off; the board and Orbit are given the URL.

## 3. Known limitations in the pilot

- **The board's own pages through the hub** (`/api/*` proxy) do not work: the
  hub reaches a board's HTTP API by its LAN address. Fails fast with a clear
  error. Later: carry those requests over the board's WebSocket (separate
  task, needed before the server hub is the main one).
- **Downlink** stays PCM 24 kHz (~48 KB/s per speaking board). Fine for one
  home; Opus down comes with calls (M5) or earlier if the measurements ask.
- **Resolver**: must be up and egress from home. At the time of writing
  `stream.<domain>` answered 502, so music waits for it.

## 4. Changes

### 4.1 Hub (nexus)

1. `--resolve-only`: no music cache whatever `--cache-max-mb` says, never
   fetch or serve song bytes, refuse to start without a resolver
   (`--music-api` / `--invidious-url` / MCP). Logged at start.
2. Timing, so the pilot can be compared with the home hub:
   - Edge round trip: WebSocket ping every 10 s per edge; keep the last,
     min, average; show them in `/hub/status` per edge.
   - Per reply: time from the last mic audio frame of the turn to Gemini's
     first reply audio at the hub, and to the first audio sent to the edge.
     One log line per reply (add to the existing reply line) and the last
     ten in `/hub/status`.
3. The `/api/*` proxy fails fast (2 s dial timeout, 502 with "board not
   reachable from the hub") instead of hanging.
4. Deployment files, no secrets, placeholder domain:
   - `deploy/nexus-hub.service` (systemd, own user, `127.0.0.1:8380`,
     `--mdns=false --resolve-only`, files under the user's config dir).
   - `deploy/nginx-hub.conf` (section 2; basic auth on everything except
     `/hub/edge` and `/hub/board`).
   - `docs/deploy-server.md`: build on the server (Go, `libopus-dev`,
     `pkg-config`: the hub uses cgo for Opus), copy the source
     (`git archive`), create the token and Gemini key files (mode 600),
     certbot, htpasswd, start, check, roll back. Also: point the home hub's
     edges back (rollback).

### 4.2 Orbit (nexus)

- `--hub wss://hub.<domain>/hub/edge` works (TLS with the system roots,
  certificate checked). Test with a TLS test server. Reconnect with backoff
  as today.

### 4.3 Board (waveshare)

- `POST /api/hub` accepts `wss://` URLs as well as `ws://`.
- `wss://` connects with the certificate bundle and the hostname check
  (`Tls::secure` / `esp_crt_bundle_attach`), like every other HTTPS client on
  the board; no unverified fallback.
- RAM: in hub mode the board has no Gemini TLS socket, so one TLS socket to
  the hub replaces the one to Google (direct mode). Still measure: internal
  RAM idle, during a session, and during a session with music streaming over
  HTTPS, against the same with `ws://`. TLS buffers in PSRAM if the config
  allows. The hub_ws task stack must hold a TLS handshake: measure its high
  water mark.
- Auto mode: if the server hub is unreachable the board falls back to direct
  Gemini as today.

## 5. Order

1. Hub and Orbit changes (AGY), reviewed.
2. The person sets up the server with the runbook; Orbit at home joins over
   `wss://` (music once the resolver is up).
3. Board `wss://` (AGY), reviewed, OTA, RAM measured (Claude).
4. Voice sessions through the server hub: compare reply delay and link
   health with the home hub (same phrases, `tools/voice_test`).
5. Decide: server hub as main (then M1 on it, the board pages over the
   WebSocket, Opus down if needed) or a home hub with WireGuard.

## 6. Measurements to record

| | Home hub | Server hub |
|---|---|---|
| Edge round trip (ms, min/avg) | | |
| Last mic frame → first reply audio at the hub (ms, median of 10) | | |
| Last mic frame → first audio sent to the board (ms) | | |
| Link warnings per 10 sessions | | |
| Board internal RAM idle / session / session + music (KB) | | |
