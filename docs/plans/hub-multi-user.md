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
- A session on a device runs as the owner unless a user switches to their
  own account (section 6).

## 3. Claiming a device

Today every edge uses one shared token. That goes away.

1. A new device connects with no token, or with the hub's **claim token**
   (a hub-wide secret printed by the hub's setup, entered once in the
   board's settings the way the hub URL is today). The hub accepts the hello,
   marks the device `unclaimed`, and answers only with a `welcome` that says
   so (`"claimed": false`). No voice, music or state.
2. Every signed-in user's dashboard shows it: "New device: Nexus (esp32-s3)".
3. A user clicks **Claim**. The hub sends `claim_confirm` to the device:
   - ESP board: LED pattern plus a chime; the person presses the action key
     within 60 s.
   - Devices without a key (Orbit, the app): the device shows a 6-digit
     code (log line, notification, screen); the person types it in the
     dashboard.
4. On confirmation the hub creates the device's own token, sends it in
   `claimed {token}`, and stores only its hash. The device saves it (NVS on
   the board) and reconnects with it.
5. Removing a device deletes its token; it is unclaimed on its next connect.

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
- Admin only: users, hub settings, logs, updates, the claim token.

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
- **Explicit switching.** On a shared device, "it's Riya" calls a hub tool
  `switch_user {name}`:
  - Allowed only to users the device is shared with (and the owner).
  - The rest of the session uses Riya's memory, contacts, MCP tools and
    music. Next session starts as the owner again.
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
| Admin | Users, claim token, hub settings, logs, update |

`web/apps/device` (the board's own dashboard) stays for standalone use.

## 9. Where the hub runs

| | Home (PC, Pi) | Server (Lightsail) |
|---|---|---|
| Devices in other homes | need remote access (below) | connect directly |
| Music | resolved and cached at home, served on the LAN | YouTube URLs are bound to the server's IP: the hub serves every song from its cache over the internet |
| Discovery | mDNS | the board needs the hub URL |
| Always on | Pi yes, PC no | yes |

Plan: keep it location-agnostic. Try Lightsail as the main hub first and
measure voice delay and music start; fall back to a home hub (with a PC or
Pi) if music over the internet is poor.

**Remote access to a home hub:** the hub keeps a WireGuard link to Lightsail;
Caddy on Lightsail serves `hub.<domain>` over HTTPS and forwards to the hub
through it. No router port forwarding (works behind CGNAT), no third-party
service.

## 10. Calls (Nexus to Nexus)

### 10.1 What the person does

- "Call Riya": hub tool `call {contact}`. Rings Riya's devices (her own, and
  shared ones where she turned that on). First to answer takes it.
- Incoming call on a board: ring tone + LED pattern, then a short Gemini
  session: "Riya is calling. Answer?"
  - "Yes" (or the action key): connect.
  - "No": decline.
  - "Tell her I'll call back in ten minutes": a voice message back (Gemini
    writes it as text, or the board records the person's reply).
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

- `hello`: `token` is the device token (or the claim token, or none).
- `welcome`: `claimed` (bool), `user` (owner's display name).
- New: `claim_confirm`, `claim_code`, `claimed`.
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

## 13. Open questions

- Claim token or open claiming on the LAN (any device may ask to be claimed)?
- Voice messages as text (Gemini) or as recordings by default?
- Does a shared device ring for calls to every user it is shared with?
- Lightsail data transfer per month with music served from the hub (measure
  in the trial).
