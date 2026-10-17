# Nexus hub protocol

Status: proto 1 (built: link, state, voice), proto 2 (built: roles, music), proto 3 (built: pairing, device tokens)
Last updated: 2026-10-03

This is the contract between `nexus-hub` and the devices that connect to it.
A new client (Android, Windows, another board) is written from this document.

## 1. The model

- **The hub is the brain.** It holds the conversation with Gemini, the tools
  (its own and MCP), the music queue, search and recommendations, the music
  cache, and which device plays.
- **Every device is an edge.** An edge does audio in and out (wake word, mic,
  speaker), plays music, and sends what the person does on it (keys, a tap).
  Edges differ only in how they do that: the ESP32 board in C++, Orbit in Go
  on Linux, Windows or a Pi, an Android app later.
- **Logic goes in the hub.** If two kinds of edge would need it, it belongs
  in the hub, not in the edges.
- **Standalone is the edge's business.** An edge may also work without a hub
  (the ESP board does everything on its own in direct mode). The protocol
  never needs that: an edge without a standalone mode just stops when the
  hub goes away.

## 2. Roles

An edge says in its `hello` which roles it takes. The hub only sends a role's
messages to edges that declared it.

| Role | The edge... | Examples |
|---|---|---|
| `voice` | streams its mic up during a session and plays the reply | ESP board; a phone or laptop later |
| `player` | plays the music the hub tells it to, reports position and track ends | ESP board, Orbit, a phone |
| `controller` | sends music actions (play X, next, pause, seek, queue edits) and gets the music state | board keys, Orbit media keys, a phone app |
| `state` | pushes its own device state and serves an HTTP API that the hub proxies for the dashboard | ESP board only |

## 3. Connection

One WebSocket per edge to `ws://<hub>:8380/hub/edge` or TLS `wss://<hub>/hub/edge` (`/hub/board` is the
same endpoint, kept for proto 1 boards). The hub advertises
`_nexus-hub._tcp` over mDNS.

- **Text frames:** JSON objects with a `type`.
- **Binary frames:** byte 0 channel, byte 1 flags, then the payload.

| Channel | Direction | Payload |
|---|---|---|
| 1 | edge to hub | mic, 16 kHz mono 16-bit little-endian PCM or length-prefixed Opus packets (flag bit 1) |
| 2 | hub to edge | voice reply, 24 kHz mono 16-bit little-endian PCM, 60 ms per frame |

**Channel 1 binary framing (edge to hub):**
- Byte 0: channel (`1` = mic).
- Byte 1: flags:
  - Bit 0 (`0x01`): reserved / keeps current meaning.
  - Bit 1 (`0x02`): uplink audio format:
    - Clear (`0`): raw 16 kHz mono 16-bit little-endian PCM payload (legacy/proto 1 default).
    - Set (`1`): length-prefixed Opus packets (16 kHz mono, `RESTRICTED_LOWDELAY` / CELT supported).
- When bit 1 is set, the frame payload contains one or more Opus packets. Each packet is preceded by a 2-byte little-endian length: `[uint16_le len][opus packet bytes]...`.
- An edge may dynamically alternate between Opus and PCM frames within the same session without reconnecting.
- Malformed frames (e.g. truncated length header or packet length exceeding frame) are dropped without terminating the session.

- **Liveness & RTT:** WebSocket ping every 10 s from the hub to edges with round-trip
  time (RTT) recorded (`last`, `min`, `avg` in `/hub/status` per edge).
  Browser connections ping every 5 s. No pong for 10 s, or a closed socket,
  means the link is down. These are protocol pings, not JSON.
- **Reconnect:** the edge reconnects with backoff (1 s doubling to 30 s).
  After `rejected` it waits 5 minutes or for a settings change.
- **One connection per id:** a second connection with the same `id` replaces
  the first.

### 3.1 hello, claimed, and welcome

#### Edge hello frame:

**1. Paired edge (Protocol 3):**
```json
{"type": "hello", "proto": 3, "id": "nexus-3c84", "name": "Kitchen",
 "kind": "esp32-s3", "fw": "1.3.0", "token": "<device-token>",
 "roles": {
   "voice": {"up": ["opus16", "pcm16"], "down": ["pcm24"]},
   "player": {"url": true, "resolves": false, "queue_copy": true},
   "controller": true,
   "state": {"http_port": 80}
 }}
```

**2. First-time edge pairing (Protocol 3):**
```json
{"type": "hello", "proto": 3, "id": "nexus-3c84", "name": "Kitchen",
 "kind": "esp32-s3", "fw": "1.3.0", "pair_code": "482-913",
 "roles": {
   "voice": {"up": ["opus16", "pcm16"], "down": ["pcm24"]},
   "player": {"url": true, "resolves": false, "queue_copy": true},
   "controller": true,
   "state": {"http_port": 80}
 }}
```

- `id`: stable per device. `name`: what people call it ("play it in the kitchen").
- `kind`: `esp32-s3`, `orbit`, `android`, ... (for logs and the dashboard).
- `token`: 32-byte base64url device token, bound to the device's `id`. The hub stores only its SHA-256 hash.
- `pair_code`: 6-digit code obtained from `POST /hub/api/pairing` (dash optional). The hub checks code hash with salt pepper and rate limiting.
- `roles.voice.up`: optional preference list of supported mic uplink formats, e.g. `["opus16", "pcm16"]`. Missing list defaults to `["pcm16"]`.
- `player.url`: it can play a stream URL the hub sends. `player.resolves`: it can play from video ID. `player.queue_copy`: keeps queue copy without hub.
- A proto 1 hello has no `roles`. The hub treats it as the ESP board: `voice`, `state` (with `http_port` from hello) and nothing else.

#### Hub handshake responses:

**1. Claimed frame (sent on pair_code before welcome):**
```json
{"type": "claimed", "token": "...", "owner": "Alice"}
```
The edge must save this token persistently in non-volatile storage (NVS) for subsequent reconnections.

**2. Welcome frame:**
```json
{"type": "welcome", "hub": "nexus-hub 0.3.0", "proto": 3,
 "voice": {"up": "opus", "down": "pcm", "down_rate": 24000},
 "music": true,
 "claimed": true,
 "owner": "Alice"}
```
- `voice`: audio format negotiation result (`opus` or `pcm`), or `false` when voice is disabled.
- `music`: true when music subsystem is enabled on the hub.
- `claimed`: true if the device is claimed by an owner.
- `owner`: display name of the device owner.

**3. Legacy edges (`--legacy-tokens`):**
Proto 2 edges connecting with shared tokens from `--token-file` receive a welcome response with `"legacy": true` and are assigned to the first admin.
```json
{"type": "welcome", "hub": "nexus-hub 0.3.0", "proto": 2,
 "voice": ...,
 "music": true,
 "legacy": true,
 "owner": "Admin"}
```
If `--legacy-tokens=false` is set on the hub, legacy edges sending shared tokens are rejected.

**4. Rejected frame:**
```json
{"type": "rejected", "reason": "bad token"}
```
The WebSocket closes with status code `4001`. Generic rejection reasons:
- `"bad token"`: device token is invalid, unrecognized, or bound to a different device ID.
- `"bad code"`: pairing code is invalid, expired, already used, or rate limit exceeded.
- `"pairing required"`: neither token nor pair code provided on edge hello.

## 4. Voice

As built in proto 1. Edge to hub:

| Message | Content |
|---|---|
| `session_start` | `setup` (optional), `web_search`, `keepalive_s`, `resume_min`, `text` (an optional first text turn) |
| mic audio | channel 1 |
| `text_turn` | `text` |
| `tool_response` | `id`, `result` (object) |
| `interrupt` | the person talked over the reply |
| `session_end` | the session is over; the hub may keep Gemini open for `keepalive_s` |
| `voice_activity` | `active`: a session the hub doesn't carry started or ended on this edge (the ESP board in direct mode), so the hub can pause music on another edge |

Hub to edge:

| Message | Content |
|---|---|
| `session_ready` | `resumed` |
| reply audio | channel 2, paced at most 1.5 s ahead of playback |
| `transcription` | `role` (`user` or `model`), `text` |
| `tool_call` | `id`, `name`, `args`: a tool the edge declared |
| `reply_owed` | a hub tool is running; a reply is still coming |
| `turn_complete`, `interrupted` | in order after the audio before them |
| `end_after_reply` | after a hub music tool succeeded (once per turn): the edge ends the session once the current reply has played, as after a music command; optional, an edge may ignore it |
| `session_error` | `reason`; the edge ends the session |

**Setup.** `setup` is the Gemini Live setup the edge wants (model, voice,
prompt, its own tool declarations). The ESP board sends it so its prompt,
memory and settings are the same in both its modes. Without `setup` the hub
uses its own stored setup (not built yet). Either way the hub adds Google
Search, the MCP tools, its own tools and the resumption handle.

**Tools.** Three kinds, each called by whoever owns it:

| Owner | Tools | How |
|---|---|---|
| hub | music: `play`, `playback`, `music_output` | run in the hub's music controller; an edge's declarations with these names are dropped from its setup |
| MCP | whatever the MCP server offers | the hub calls the MCP server |
| edge | the rest of what the edge declares (alarms, volume, recordings ...) | `tool_call` to the edge, `tool_response` back |

## 5. Music

The hub owns the queue and picks the output: one player at a time.

### 5.1 Hub to player

| Message | Content |
|---|---|
| `music_play` | `track` {`id`, `title`, `artist`, `duration`}, `url` (when the player wants one and the hub has it), `position_ms`, `paused`, `seq` |
| `music_pause`, `music_resume`, `music_stop` | |
| `music_seek` | `position_ms` |
| `music_queue` | `tracks`: what plays next, in order (up to 20), `repeat`, `autoplay`; only to players with `queue_copy` |
| `music_url` | `id`, `url`: answer to `music_renew` |

- `seq` numbers each `music_play`. Reports and events carry the `seq` they
  belong to, so the hub ignores a late `ended` from the track before.
- A player that has the song saved (the board's card) plays it from there
  and ignores `url`.
- `url` may be the hub's own copy, `http://<hub>/hub/cache/<id>.webm`
  (plain HTTP on the LAN, Range requests for seeking), or a YouTube stream
  URL. A player treats both the same.
- Stream URLs are signed, bound to the home IP and expire (~6 h). Nobody
  logs them (log the host) or stores them.
- No `url` and the player can't resolve: it sends `music_renew`.

### 5.2 Player to hub

| Message | Content |
|---|---|
| `music_report` | `seq`, `id`, `state` (`buffering`, `playing`, `paused`, `idle`), `position_ms`, `duration_ms`: on change and every second while playing |
| `music_event` | `seq`, `id`, `event`: `ended`, or `error` with `reason` (`not_found` means skip it; anything else may be retried) |
| `music_renew` | `id`: it needs a fresh URL (expired after a long pause, or none sent) |

During a voice session on the same edge, the edge pauses its own music and
resumes it after, as it does without a hub; its reports show `paused`. If the
music plays on another edge, the hub pauses that edge for the session and
resumes it after (`session_start`/`session_end`, or `voice_activity`).

### 5.3 Controllers

Controller to hub, `music_action`:

| `action` | Arguments |
|---|---|
| `play`, `play_next`, `queue` | `query` (search) or `id` (a known song; `cached: true` if the edge has it saved) |
| `next`, `previous`, `pause`, `resume`, `toggle`, `stop`, `replay` | |
| `seek` | `position_ms` |
| `remove` | `index` in the queue |
| `clear`, `shuffle` | |
| `repeat` | `mode`: `off`, `one`, `all` |
| `autoplay` | `on` |
| `output` | `target`: a player's id or name (a name may be partial) |

Hub to controllers, `music_state` on every change:

```json
{"type": "music_state", "output": "nexus-3c84", "state": "playing",
 "track": {"id": "...", "title": "...", "artist": "...", "duration": 212},
 "position_ms": 41000, "queue": [...], "repeat": "off", "autoplay": true,
 "players": [{"id": "nexus-3c84", "name": "Kitchen", "kind": "esp32-s3"}]}
```

Actions are answered through `music_state` (and `music_play` to the player),
not one by one. The hub decides; an edge that is both player and controller
does not act on its own key press, it sends it and plays what comes back.

### 5.4 Hub away

- A player with `queue_copy` carries on: the current song keeps playing (its
  bytes don't come through the hub), and at its end it plays from its copy
  of the queue. Without `queue_copy` it stops when the song ends.
- When the link comes back, a player that played on its own sends
  `music_sync` first: `id`, `state`, `position_ms`, `queue`. The hub takes
  that as the music state (the edge's current song wins) and carries on from
  there.
- Music on a player without `queue_copy` stops with the hub.

## 6. Device state (the `state` role)

The ESP board only. It pushes its state JSON (no `type`) on change and every
second, and `transcript` messages; the hub fans them out to browsers on its
own `/api/ws`, and forwards a browser's `{"cmd": ...}` to the board as is.
The hub proxies `/api/*` to the board's HTTP server (its address as the hub
sees it, port `state.http_port`) with a 2-second dial timeout. If the board is
unreachable, the proxy returns HTTP 502 with `{"status":"error","message":"board not reachable from the hub"}`.

## 7. Versions

`proto` goes up when a message changes meaning. The hub accepts every
version it knows and answers with the lowest of the two. New optional fields
and new message types do not change the version: both sides ignore types and
fields they don't know.

## 8. Hub HTTP API & Authentication (`/hub/api/*`)

All JSON, under `/hub/api/`. Unauthenticated requests to protected endpoints return `401 Unauthorized` (`{"error": "unauthorized"}`).

### 8.1 Authentication & Sessions
- **Session Cookie**: `nexus_session` stores a 32-byte base64url token. The SQLite database stores only the SHA-256 checksum (`sessions.id_hash`).
- **Cookie Attributes**: `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` when served over direct HTTPS or via trusted reverse proxy (`--trusted-proxy`). Sliding idle expiration of 30 days.
- **CSRF Protection**: All state-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`) require `Content-Type: application/json` and an `Origin` (or `Referer`) matching the hub's host or `--public-url`.
- **Rate Limits**:
  - Login: 5 failed attempts per username or IP in 15 minutes triggers a 15-minute lockout (returns `401 {"error": "invalid username or password"}`).
  - Setup code: 10 failed attempts per IP in 10 minutes triggers `429 Too Many Requests`.
  - Constant-time auth delays: `/hub/api/setup`, `/hub/api/login`, and `/hub/api/me/password` take at least 300 ms.

### 8.2 Endpoints Table

| Method & Path | Access | Description |
|---|---|---|
| `GET /hub/api/setup` | Public | Returns `{"needed": true/false}` depending on whether any users exist. |
| `POST /hub/api/setup` | Public (first run only) | `{setup_code, username, display_name, password}` → Creates first admin, starts session. |
| `POST /hub/api/login` | Public | `{username, password}` → Verifies credentials, sets `nexus_session` cookie, returns `{user}`. |
| `POST /hub/api/logout` | Authenticated | Deletes session in DB and clears cookie. |
| `GET /hub/api/me` | Authenticated | Returns `{id, username, display_name, role, must_change}`. |
| `POST /hub/api/me/password` | Authenticated | `{current, new}` → Updates password, revokes all other sessions. |
| `GET /hub/api/users` | Admin only | Returns list of all users. |
| `POST /hub/api/users` | Admin only | `{username, display_name, role}` → Creates user with temporary password and `must_change: true`. |
| `POST /hub/api/users/{id}/reset` | Admin only | Generates new temporary password, updates `must_change: true`, revokes user's sessions. |
| `DELETE /hub/api/users/{id}` | Admin only | Deletes user (cannot delete self or last admin). |
| `POST /hub/api/pairing` | Authenticated | Issues a 6-digit pairing code with 10-minute expiry (max 3 live codes per user). Returns `{code: "482-913", expires_at}`. |
| `GET /hub/api/pairing` | Authenticated | Returns active pairing codes for current user: `[{code, expires_at}]`. |
| `DELETE /hub/api/pairing/{code}` | Authenticated | Cancels a specific pairing code. |
| `GET /hub/api/devices` | Authenticated | Returns user's devices (or all devices if admin), with fields `{id, name, kind, online, fw, roles, uplink, last_seen, connected_s, owner, legacy}`. |
| `PATCH /hub/api/devices/{id}` | Owner or Admin | `{name}` → Renames device. |
| `DELETE /hub/api/devices/{id}` | Owner or Admin | Removes device from DB and disconnects any connected edge with that ID. |

### 8.3 Board API Proxies
- `/hub/d/{id}/api/*`: Reverse proxy to edge device's HTTP server. Restricted to device owner (or admin). Enforces session authentication, `must_change` check, and CSRF Origin check on state-changing methods.
- `/api/ws` and `/api/*`: Legacy board API proxy and browser WebSocket. Restricted to the owner of `h.board` (or admin).

### 8.4 Other Routes Auth Policy
- `/hub/status`: Admin only (`401` if unauthenticated, `403` if not admin).
- `/hub/cache/*`: Open on LAN for edge audio streaming.

