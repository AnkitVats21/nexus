# Hub: users, devices and calls

Status: design, agreed in outline 2026-10-03; nothing built yet
Builds on: `docs/PROTOCOL.md` (proto 2), `DESIGN.md`

## 1. Goals

- Several people use one hub. Each has an account with their own devices,
  memory, voice setup, MCP servers and music.
- A device is attached to a person by claiming it, with proof that the person
  is at the device.
- A device can be shared: a kitchen board that everyone in the house uses.
- On a shared device, a person can say who they are ("it's Riya") and the
  session switches to their account.
- Nexus-to-Nexus voice calls: "call Riya" rings Riya's devices.
- The hub runs on a home machine (PC, Pi) or on a server (Lightsail): same
  binary, same code.

Not goals: calls to phone numbers or WhatsApp (dropped: providers, cost and
regulation), voice recognition of who is speaking, federation between hubs.

## 2. The model

| Thing | What it is |
|---|---|
| User | An account: name, login, role (`admin` or `member`). The first user created is the admin. |
| Device | An edge (`PROTOCOL.md` §2): ESP board, Orbit, the app later. Has an id, a name, a kind, its roles, one owner, and its own token. |
| Share | Owner lets another user use a device: play music on it, talk to it as themselves, call it. |
| Contact | A user's address book entry: another user on this hub, with a nickname ("Mom"). |
| Account data | Per user: memory, voice setup (model, voice, prompt, VAD), MCP servers and their tokens, music history and library, contacts, call history, voice messages. |

Rules:
- A device has exactly one owner. Unclaimed devices belong to nobody and can
  do nothing but say hello.
- A user sees and controls their own devices and the ones shared with them.
- A device is bound to one user at a time, the owner unless someone
  switched (section 6).

## 3. Claiming a device

Today every edge uses one shared token. That goes away.

Pairing works like pairing a TV with a streaming account: a one-time code
from the user, entered at the device.

1. In the dashboard the user clicks **Add device**. The hub shows a pairing
   code (`482-913`), valid for 10 minutes, tied to that user.
2. The person enters the hub address and the code once at the device: the
   board's own settings page or its Wi-Fi setup portal; a flag or prompt for
   Orbit; a screen in the app.
3. The device connects with the code in its hello. The hub checks it, claims
   the device into that user's account, creates the device's own token,
   sends it in `claimed {token}` and stores only its hash. The code is used
   up. The device saves the token (NVS on the board) and uses it from then
   on.
4. A hello with no valid token or code is rejected (`rejected`). There is no
   list of unknown devices, so nothing on the internet can show up in a
   dashboard or be claimed by mistake. No hub-wide secret exists.
5. Optional confirmation on the board: LED pattern and a key press before
   the claim completes.
6. Removing a device deletes its token; it needs a new code to come back.

The board keeps working without a hub (standalone mode) in every state.

## 4. Logins and access

- Password login (argon2id hash), session cookie (`HttpOnly`, `Secure` when
  served over HTTPS, `SameSite=Lax`), CSRF token on state-changing requests.
- Rate limit and lock-out on failed logins from the start, so remote access
  needs no rework.
- Passkeys later.
- Every API call is checked against the user: devices, music, settings and
  data of other users are not visible. The `/api/*` proxy to a board
  (`PROTOCOL.md` §6) is allowed only for users who own or share that board.
- Admin only: users, hub settings, logs, updates.

## 5. Storage

- One SQLite file on the hub (`nexus.db`, WAL mode). Small enough for a Pi,
  one file to back up.
- Secrets (MCP tokens, per-user API keys) are encrypted with a key in a file
  only the hub reads (`~/.config/nexus/hub_secret`, mode 600), never in the
  database in clear and never logged.
- Memory and transcripts are user data: kept per user, deletable with the
  account.

Tables (first cut): `users`, `sessions`, `devices`, `device_shares`,
`settings` (per user, key/value JSON), `memory`, `mcp_servers`, `contacts`,
`calls`, `voice_messages`, `music_history`.

## 6. Voice sessions per user

- **The hub builds the setup.** Today the board sends its whole Gemini setup
  (~13 KB). With accounts, the hub builds it from the session user's voice
  setup, memory and tools. The board sends only its own tool declarations
  (alarms, volume, recordings ...) and device facts (name, time zone).
- **The device's current user.** Each device is bound to one user at a
  time: the owner by default. Sessions, music started by voice and incoming
  calls on the device follow that user.
- **Explicit switching.** On a shared device, "it's Riya" calls a hub tool
  `switch_user {name}`, which binds the device to Riya:
  - Allowed only to users the device is shared with (and the owner).
  - Sessions use Riya's memory, contacts, MCP tools and music until someone
    else switches, or the device goes back to its owner after a period
    without use (setting, default 2 h). The dashboard shows who each device
    is bound to.
  - Risk: anyone in the room can claim to be Riya. Mitigations: each user
    turns voice switching on for themselves (off by default); tools that act
    for the person (calls, messages, MCP tools marked sensitive) ask for a
    spoken confirmation; the dashboard lists sessions run as you, per device.
- **Memory on claim.** A board that ran standalone has memory on its card.
  At claim the hub offers to import it into the owner's account. After that
  the hub is the source of truth while connected; the board's copy is used
  only in standalone mode.

## 7. Music per user

- One music controller per user (today: one per hub). Its outputs are the
  devices the user may use.
- A shared device plays one user's music at a time: the last user to start
  music there takes it; the other user's queue stays, paused.
- History, library, recommendations per user.

## 8. Dashboard (web/apps/hub)

| Page | Contents |
|---|---|
| Sign in | Login; first-run creates the admin |
| Devices | Own and shared devices: link health, firmware, roles, uplink; claim new ones; share; remove; open a board's own pages through the proxy |
| Music | Queue, now playing, output picker over the user's devices |
| Assistant | Memory, voice setup, session history and transcripts |
| Tools | Built-in hub tools on/off, MCP servers (add, token, test), later a registry |
| Contacts and calls | Contacts, call history, voice messages |
| Admin | Users, hub settings, logs, update |

`web/apps/device` (the board's own dashboard) stays for standalone use.

## 9. Where the hub runs

| | Home (PC, Pi) | Server (Lightsail) |
|---|---|---|
| Devices in other homes | need remote access (below) | connect directly |
| Music | resolved and cached at home, served on the LAN | resolved only: the server never carries or caches music (below) |
| Discovery | mDNS | the board needs the hub URL |
| Always on | Pi yes, PC no | yes |

Plan: keep it location-agnostic. Try Lightsail as the main hub first and
measure voice delay; fall back to a home hub (PC or Pi) if it is poor.

**Music on a server hub: resolve only.** The server never streams or caches
music (data transfer). It only resolves a song to a stream URL. Stream URLs
are bound to the IP that resolved them, so resolution goes out through a
proxy in the home (arranged separately), and the players fetch the song
straight from YouTube. The music cache then lives on the players (the
board's SD card), not on the hub. A home hub may keep its LAN cache.
Later, separately: devices as resolvers themselves, or as the proxy the
server resolves through.

**Remote access to a home hub:** the hub keeps a WireGuard link to Lightsail;
Caddy on Lightsail serves `hub.<domain>` over HTTPS and forwards to the hub
through it. No router port forwarding (works behind CGNAT), no third-party
service.

## 10. Calls (Nexus to Nexus)

### 10.1 What the person does

- "Call Riya": hub tool `call {contact}`. Rings the devices currently bound
  to Riya (section 6), since that is where she is. First to answer takes
  it.
- **Several devices ring at once**, like FaceTime or WhatsApp:
  - The first to answer takes the call; the others stop ringing at once and
    show "answered on Kitchen".
  - Declining on any device declines everywhere; the caller is offered a
    voice message.
  - No answer in 30 s: missed on all, voice message offered.
  - Skipped: a device already in a call, or in its do-not-disturb hours
    (per device, so the bedroom speaker can stay quiet at night while the
    phone still rings). A device in a voice session plays a soft tone and the
    assistant says who is calling once its reply ends.
  - The ring plays over music; answering pauses the music on that device and
    it resumes after the call.
- **Moving a live call:** "move the call to the kitchen" (or a tap in the
  app) switches which device carries the call's audio without hanging up,
  the way music moves between outputs.
- Incoming call on a board: ring tone + LED pattern, then a short Gemini
  session: "Riya is calling. Answer?"
  - "Yes" (or the action key): connect.
  - "No": decline.
  - "Send her a message": the board records the person's reply and the hub
    delivers it as a voice message (a recording, not text).
- No answer in 30 s: missed call, and the caller can leave a voice message
  ("leave a message" / the hub offers it).
- Missed calls and messages: told at the next session ("You missed a call
  from Riya"), and listed in the dashboard and the app.

### 10.2 Hub

- Call signalling in the hub: `call_invite`, `call_ring`, `call_answer`,
  `call_decline`, `call_end`, with a call id; states and timeouts in the hub.
- Audio: the hub forwards edge A's mic (channel 1) to edge B's speaker
  (channel 2) and back, decoding/encoding as each side negotiated. A call is
  a third kind of session beside voice and music, same channels.
- Downlink format: PCM on the LAN as today; Opus down when either side is
  remote (section 10.3).
- Who may call whom: contacts on the same hub. Do-not-disturb hours per user.

### 10.3 Board (firmware)

- **Call mode** in the audio pipeline: mic and speaker at the same time, AEC
  on (tested 2026-10-03: good enough for calls), no wake word, no VAD end.
- Ring and answer flow (above), action key answers and hangs up.
- Voice-message recording: reuses the recorder.
- **Opus downlink** for remote calls: an Opus decoder on the board (24 kHz
  mono). Needs an internal-RAM stack (decode on a PSRAM stack is 5-10x
  slower, `waveshare/AGENTS.md`): measure the internal RAM cost before
  building.
- Music pauses for a call as it does for a voice session.

### 10.4 App

Later: the app is one more edge with `voice` and a `call` role; calling home
from the phone works the same way.

## 11. Protocol changes (proto 3)

- `hello`: `token` is the device token, or `pair_code` on first connect.
- `welcome`: `claimed` (bool), `user` (owner's display name).
- New: `claimed {token}`, optional `claim_confirm` (key press on the board).
- `session_start`: the board sends its tool declarations and device facts
  instead of the whole setup (proto 2 boards keep sending the setup; the hub
  accepts both during the change).
- New role `call`; messages in 10.2.

## 12. Milestones

| | What | Done when |
|---|---|---|
| M1 | Users, login, claiming, device tokens, Devices page | Two users on one hub; each claims a board; neither sees the other's; the old shared token is gone |
| M2 | Per-user assistant: the hub builds the setup; memory, voice setup per user; memory import at claim; explicit switching | A session on a shared board runs as the owner, and as Riya after "it's Riya" |
| M3 | Per-user tools: MCP servers per user, encrypted tokens; Tools page | One user's MCP server is not offered in another's session |
| M4 | Sharing and music per user: shares, per-user music controller and outputs, history | Two users play different music on their own boards; a shared board plays the last starter's |
| M5 | Calls: signalling, LAN calls between two boards, ring/answer via Gemini, voice messages, missed calls; then Opus down and remote calls | Two boards on one hub call each other with AEC; a declined call leaves a voice message |

Each milestone: plan reviewed first, hub tests, the board unchanged in
standalone mode.

## 12a. Hub App Auth (Android Companion App Authentication) - As Built

Implemented in `task-037` to support native Android app companion authentication, pairing, and token management:

### 1. Ed25519 JWT Access Tokens
- **Key Management**: Ed25519 private key stored in PEM format (`--jwt-key`, default `~/.config/nexus/jwt_ed25519.pem`, permissions `0600`). Hub auto-generates key if missing.
- **Key ID (`kid`)**: First 8 bytes of `SHA256(pubkey)` encoded as a 16-character hex string.
- **Token Format**: Standard RFC 7519 JWT signed with `EdDSA`.
- **Claims**:
  - `iss`: `"nexus-hub"`
  - `aud`: `"nexus"`
  - `sub`: user ID formatted as string
  - `preferred_username`: user's username
  - `role`: user role (`admin` or `member`)
  - `exp`: 15 minutes from issue time (with 30-second leeway on validation)
  - `iat`: issued-at timestamp
  - `jti`: CSPRNG UUID
- **JWKS Endpoint**: `GET /hub/api/auth/jwks` exposes RFC 7517 / RFC 8037 JWKS containing `kty: "OKP"`, `crv: "Ed25519"`, `kid`, `x` (base64url-encoded public key).

### 2. Rotating Refresh Tokens & Reuse Detection
- **Token Format**: 32-byte CSPRNG base64url string (30 days validity).
- **Storage**: SQLite `refresh_tokens` table with SHA-256 hash (raw tokens are never stored or logged), `family_id` UUID, `user_id`, `device_id` (optional FK with SET NULL), `expires_at`, `used_at`.
- **Rotation**: Calling `POST /hub/api/auth/refresh` with an active token issues a new access token and rotating refresh token in the same family, marking the old refresh token as `used_at`.
- **Replay / Reuse Detection**: Presenting an already-used refresh token triggers automatic compromise detection: deletes the entire family (`DeleteRefreshTokensByFamily`) and returns `401 Unauthorized`.
- **Revocation**: `POST /hub/api/auth/logout` deletes the entire family. Disabling a user or resetting their password purges all refresh tokens for that user.

### 3. Self Sign-Up & Admin Approval Lifecycle
- **Sign-Up**: `POST /hub/api/register` creates user with `status="pending"`. Gated by `--allow-signup` flag (default `true`; `403 Forbidden` if disabled) and setup status (`409 Conflict` before initial setup).
- **Rate Limiting**: Sliding 1-hour window per client IP, max 5 registrations / hour (`429 Too Many Requests`).
- **Gating**:
  - Pending user login -> `403 Forbidden` with `{"error":{"code":"pending_approval"},"code":"pending_approval"}`.
  - Disabled user login -> `403 Forbidden` with `{"error":{"code":"disabled"},"code":"disabled"}`.
  - Returned ONLY after password verification to avoid credential / status probing (invalid passwords always return `401 Unauthorized`).
- **Admin Management**:
  - `POST /hub/api/users/{id}/approve`: pending -> active.
  - `POST /hub/api/users/{id}/reject`: pending only -> deletes user record (`400 Bad Request` if user is active).
  - `POST /hub/api/users/{id}/disable`: marks status as `disabled`, terminates active web sessions, deletes all refresh tokens, terminates live edge connections immediately. Edge reconnect hello rejected with `{"type": "rejected", "reason": "disabled"}` (device token preserved). Last active admin cannot be disabled (`400 Bad Request`).
  - `POST /hub/api/users/{id}/enable`: disabled -> active.
- **Admin Protection**: `CountActiveAdmins` ensures at least one active admin exists before delete or disable operations.

### 4. App Device Registration
- `POST /hub/api/devices/register` with Bearer auth:
  - Enforces `kind="android"` (rejects other kinds with `400 Bad Request`).
  - Accepts alphanumeric + hyphen IDs (max 64 chars) or auto-generates `nexus-app-<12 hex digits>`.
  - Rotates device token if device is already owned by the requesting user.
  - Returns `409 Conflict` if device ID is registered to another user.
  - Binds refresh family to device ID if optional `refresh_token` is supplied in the request body (must be caller's own, unused, and unexpired token; foreign/used/expired/unknown tokens answer `400 Bad Request` and bind nothing).
  - Returns 32-byte CSPRNG `device_token` (hash stored in DB).

### 5. Bearer Auth & CSRF Rules
- Bearer JWT accepted across all `/hub/api/*` endpoints.
- Store user status checked dynamically on every request: disabled user access token returns `401 Unauthorized` even if JWT has not expired.
- **CSRF Rule**: If request carries a Bearer JWT and no session cookie, CSRF check is exempt. If a session cookie is attached, CSRF check is strictly enforced regardless of Bearer header.

## 12b. Hub Per-User Music Library API - As Built

Implemented in `task-039` to provide per-user data persistence and synchronization for companion apps and dashboard (M4 Phase A4):

### 1. Database Schema (Migration 004)
- **`tracks`**: Shared metadata snapshot (`video_id TEXT PRIMARY KEY`, `title`, `artist`, `album`, `duration_s`, `thumb_url`, `updated_at`). Upserted on every write carrying a track (`ON CONFLICT(video_id) DO UPDATE`).
- **`likes`**: User likes (`user_id`, `video_id`, `liked_at`, `PRIMARY KEY (user_id, video_id)`). Cascades on user deletion.
- **`playlists`**: User playlists (`id TEXT PRIMARY KEY` (URL-safe base64), `user_id`, `name`, `created_at`, `updated_at`, `version INTEGER NOT NULL DEFAULT 1`).
- **`playlist_items`**: Dense items (`item_id TEXT PRIMARY KEY`, `playlist_id`, `position INTEGER NOT NULL`, `video_id`, `added_at`). Maintains dense `0..n-1` ordering across insertions, moves, and deletions in atomic transactions.
- **`history`**: Playback scrobble log (`id INTEGER PRIMARY KEY`, `user_id`, `video_id`, `played_at`, `played_ms`, `device_id REFERENCES devices(id) ON DELETE SET NULL`, `source TEXT CHECK (source IN ('app','board','dashboard'))`). `UNIQUE (user_id, video_id, played_at)` prevents duplicate uploads.
- **`library_versions`**: Sync version counters (`user_id PRIMARY KEY`, `likes_version`, `playlists_version`, `history_version`). Bumped inside the mutation transaction.

### 2. Track Object & Validation
Shared across all requests and responses:
- `id`: 11-character video ID matching `^[A-Za-z0-9_-]{11}$`.
- `title`: 1 to 300 characters (trimmed).
- `artist`: 0 to 300 characters.
- `album`: Optional, 0 to 300 characters.
- `duration`: Integer seconds in range `0..86400`.
- `thumbnail`: Optional, `https://` only, max 500 characters.
- Non-conforming objects rejected with `400 Bad Request` (`{"error":{"code":"invalid_track"},"code":"invalid_track"}`).

### 3. API Endpoints (`/hub/api/me/...`)
All endpoints authenticated via `h.authenticate()` (session cookie or Bearer JWT). Writes enforced via `validateCSRFOrBearer()`. `must_change` status blocks access with `403 Forbidden`. User isolation strictly enforced: querying or modifying another user's playlist returns `404 Not Found` (never 403).

- **Likes**:
  - `GET /hub/api/me/likes?cursor=&limit=`: Newest first, opaque base64url cursor, default limit 50, max 200. ETag set from `likes_version`.
  - `GET /hub/api/me/likes/ids`: Compact array `{"ids": [...]}` for heart icons. ETag set from `likes_version`.
  - `PUT /hub/api/me/likes/{video_id}`: Idempotent like. Keeps original `liked_at`, upserts track metadata, bumps `likes_version`.
  - `DELETE /hub/api/me/likes/{video_id}`: Idempotent unlike. Bumps `likes_version`, answers `204 No Content`.
- **Playlists**:
  - `GET /hub/api/me/playlists`: Returns user's playlists with `id`, `name`, `count`, `version`, `updated_at`, and up to 4 item thumbnails. ETag set from `playlists_version`.
  - `POST /hub/api/me/playlists`: `{name}` (1-100 characters trimmed). Max 500 playlists per user (exceeding answers `422 Unprocessable Entity` `{"error":{"code":"limit"}}`). Answers `201 Created`.
  - `GET /hub/api/me/playlists/{id}?cursor=&limit=`: Returns playlist metadata and items with `item_id`, `position`, `added_at`, `track`. ETag set from playlist `version`.
  - `PATCH /hub/api/me/playlists/{id}`: `{name, version?}`. Version mismatch answers `409 Conflict` `{"error":{"code":"version_conflict"},"current_version":v}`. Returns new version.
  - `DELETE /hub/api/me/playlists/{id}?version=`: Deletes playlist and items, bumps `playlists_version`, answers `204 No Content`.
  - `POST /hub/api/me/playlists/{id}/items`: `{tracks:[...], position?, version?}`. Max 5000 items per playlist (exceeding answers `422 limit`). Maintains dense `0..n-1` positions. Returns `{"version": new_version, "added": n}`.
  - `DELETE /hub/api/me/playlists/{id}/items/{item_id}?version=`: Deletes item and re-indexes dense `0..n-1`. Returns `{"version": new_version}`.
  - `POST /hub/api/me/playlists/{id}/move`: `{item_id, to, version?}`. Reorders item to `to` index and densely re-indexes `0..n-1`. Returns `{"version": new_version}`.
- **History**:
  - `POST /hub/api/me/history`: `{entries: [...]}` up to 200 per call (exceeding answers `422 limit`). Enforces time window `[-30 days, +5 min]` (out-of-window answers `400 invalid_timestamp`). Verifies device ownership against registered devices (foreign device IDs set to NULL). Duplicate `(user_id, video_id, played_at)` ignored via `INSERT OR IGNORE`. Answers `{"accepted": n}`.
  - `GET /hub/api/me/history?cursor=&limit=`: Newest first, opaque base64url cursor paging. ETag set from `history_version`.
  - `DELETE /hub/api/me/history`: Clears all history for user, answers `204 No Content`.
  - `DELETE /hub/api/me/history/{id}`: Deletes single history row, answers `204 No Content`.
- **Sync Helper**:
  - `GET /hub/api/me/library/versions`: Answers `{"likes": n, "playlists": n, "history": n}` for instant cache validation.
  - All list endpoints evaluate `If-None-Match` against the matching version and answer `304 Not Modified` on cache hits.

### 4. Rate Limiting & Retention
- **Write Rate Limiter**: 120 writes/min per user enforced across all state-changing `/hub/api/me/...` endpoints via `RateLimiter` (exceeding answers `429 Too Many Requests` `{"error":{"code":"rate_limited"}}`).
- **History Retention**: Newest 10,000 history rows kept per user, pruned periodically on hub startup and via the 6-hour maintenance ticker.

### 5. Hub-Played History Hook
- When the hub's music controller plays a song on a user's board player edge (`ownerID > 0`), it monitors playback progress:
  - If playback reaches 30 seconds (`music_report.position_ms >= 30000`), a history row with `source: 'board'` and the device ID is recorded for the owner.
  - If a track shorter than 30s ends normally (`music_event.event == 'ended'`), it records the history row upon completion.
  - Skips before 30s are not scrobbled. Each playback sequence is recorded at most once. Edge protocol remains untouched.

### 6. Out of Scope
The following components are deferred to subsequent tasks:
- Per-user music controller and outputs (independent queues per user).
- Library sharing between hub users.
- Gemini voice session tools for library ("like this song", "play my liked songs").
- Catalog search service and recommendations service.

## 13. Open questions

- The home proxy for resolving on a server hub (arranged separately).

Decided 2026-10-03: pairing code instead of a claim token; voice messages are recordings; a call rings the devices bound to
the callee; a server hub only resolves music, never carries it.
