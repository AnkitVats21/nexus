# M1: users, login, pairing, device tokens

Status: spec, ready to build (2026-10-03)
Parent: `hub-multi-user.md` (sections 2-4, 8, 11)
Builds: the hub (`pkg/hub`, new `pkg/store`, `pkg/auth`), the hub web app
(`web/apps/hub`), the board firmware (`waveshare`, pairing only), Orbit
(pairing only).

## 1. Scope

In M1:
- Users with passwords; the first user is the admin, created with a setup
  code from the hub's log.
- Login sessions for the dashboard; every hub API and the browser WebSocket
  need one.
- Pairing codes; each device gets its own token; a device belongs to the
  user who paired it.
- Devices page: list, rename, remove, add (pairing code).
- Admin: create users, reset a password, delete a user.
- The board, Orbit and the device dashboard can pair.

Not in M1 (later milestones): sharing, the device's current user and
switching, per-user memory/setup/MCP/music, calls. In M1 the music
controller and voice setup stay hub-wide as today; any signed-in user sees
the music state (documented limitation).

## 2. Storage (`pkg/store`)

- SQLite, pure Go driver `modernc.org/sqlite` (no cgo: cross-builds for the
  Pi and Lightsail). One file, default `~/.local/share/nexus/nexus.db`
  (flag `--db`), WAL mode, `foreign_keys=ON`, `busy_timeout=5000`.
- Migrations: numbered SQL in `pkg/store/migrations/NNN_name.sql`, embedded
  with `go:embed`, applied in order inside a transaction at startup, version
  in `schema_version`. Never edit an applied migration; add a new one.
- All access through `store.Store` methods (no SQL outside `pkg/store`).
  Tests use a temp file.

```sql
-- 001_users_devices.sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,  -- login name, [a-z0-9._-]{2,32}
  display_name  TEXT NOT NULL,                         -- "Riya"
  role          TEXT NOT NULL CHECK (role IN ('admin','member')),
  pass_hash     TEXT NOT NULL,                         -- argon2id encoded string
  must_change   INTEGER NOT NULL DEFAULT 0,            -- set by admin create/reset
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE TABLE sessions (
  id_hash     BLOB PRIMARY KEY,        -- sha256 of the cookie value
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,        -- sliding: extended on use, max 30 days idle
  user_agent  TEXT, ip TEXT
);
CREATE TABLE devices (
  id          TEXT PRIMARY KEY,        -- the edge's id from its hello
  owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL,
  token_hash  BLOB NOT NULL UNIQUE,    -- sha256 of the device token
  created_at  INTEGER NOT NULL,
  last_seen   INTEGER,
  last_fw     TEXT
);
CREATE TABLE pairing_codes (
  code_hash   BLOB PRIMARY KEY,        -- sha256(pepper || code)
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at  INTEGER NOT NULL,
  created_at  INTEGER NOT NULL
);
```

Times are Unix seconds (UTC). Deleting a user deletes their devices (the
devices must pair again).

## 3. Accounts and login (`pkg/auth`)

**Passwords.** argon2id (`golang.org/x/crypto/argon2`): memory 64 MiB, time
3, threads 2, 16-byte salt, 32-byte key, stored in the standard encoded
form (`$argon2id$v=19$m=65536,t=3,p=2$salt$key`). Minimum length 10. On a
Pi Zero 2W check that one hash takes < 1 s; lower memory to 32 MiB there if
not (parameters live in the encoded string, so old hashes keep working).

**First run.** With no users, the hub prints once at startup:
`First run: open http://<host>:8380/setup and use setup code 123-456`.
The code (6 digits, valid until an admin exists, regenerated per start) is
required by `POST /hub/api/setup`. This stops a stranger from creating the
admin on a hub reachable from the internet. Never log it at debug level
again or anywhere else.

**Sessions.** Cookie `nexus_session`: 32 random bytes, base64url; the
database keeps only its sha256. `HttpOnly`, `SameSite=Lax`, `Path=/`,
`Secure` when the request came over HTTPS (direct TLS or
`X-Forwarded-Proto: https` from a configured trusted proxy, flag
`--trusted-proxy`). Expiry slides on use, 30 days idle; logout deletes the
row. Changing a password deletes the user's other sessions.

**CSRF.** State-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`) must
carry `Content-Type: application/json` and an `Origin` (or `Referer`) whose
host matches the request's host (or a configured public host, `--public-url`).
No form posts exist, so this plus `SameSite=Lax` is enough.

**Rate limits.** In memory (lost on restart, fine):
- Login: per username and per IP, 5 failures in 15 min locks that key for
  15 min; the reply is the same as a wrong password (`401`, generic text).
- Setup code and pairing codes: per IP, 10 wrong codes per 10 min, then
  `429` for 10 min; globally 100 wrong pairing codes per 10 min.
- All auth replies take at least 300 ms (no timing hints).

**Roles.** `admin`: everything. `member`: own account, own devices.

## 4. HTTP API (hub)

All JSON, under `/hub/api/`. Without a valid session: `401`. Errors:
`{"error": "<short text>"}`.

| Method, path | Who | Does |
|---|---|---|
| `GET /hub/api/setup` | anyone | `{"needed": true/false}` |
| `POST /hub/api/setup` | anyone, while no users | `{setup_code, username, display_name, password}` → creates the admin, signs in |
| `POST /hub/api/login` | anyone | `{username, password}` → cookie; `{user}` |
| `POST /hub/api/logout` | user | deletes the session |
| `GET /hub/api/me` | user | `{id, username, display_name, role, must_change}` |
| `POST /hub/api/me/password` | user | `{current, new}` |
| `GET /hub/api/users` | admin | list |
| `POST /hub/api/users` | admin | `{username, display_name, role}` → `{user, temp_password}` (shown once; `must_change`) |
| `POST /hub/api/users/{id}/reset` | admin | → `{temp_password}` |
| `DELETE /hub/api/users/{id}` | admin | not the last admin, not yourself |
| `GET /hub/api/devices` | user | own devices (admin: all, with owner): `{id, name, kind, online, fw, roles, uplink, last_seen, connected_s, owner}` |
| `PATCH /hub/api/devices/{id}` | owner/admin | `{name}` |
| `DELETE /hub/api/devices/{id}` | owner/admin | removes it; closes its connection |
| `POST /hub/api/pairing` | user | → `{code: "482-913", expires_at}`; at most 3 live codes per user |

Existing endpoints:
- `/hub/status`: admin only (it lists every edge).
- The browser WebSocket `/api/ws` and the `/api/*` proxy to a board: signed
  in, and the board must be one of the user's devices. Today the hub has one
  state board (`h.board`). M1 keeps that model but checks ownership; with
  more boards the proxy moves to `/hub/d/{device_id}/api/*` (the device app
  takes an API base path). Do the base path in M1 so the second board works.
- `/hub/cache/*` (music for the players): stays open on the LAN; add a check
  later if the hub is public (players will then send their device token).

## 5. Edges: tokens and pairing (protocol 3)

Hello (`PROTOCOL.md` §3.1) changes:

```json
{"type": "hello", "proto": 3, "id": "nexus-3c84", "name": "Kitchen", "kind": "esp32-s3",
 "token": "<device token>"}                 // paired
{"type": "hello", "proto": 3, "id": "nexus-3c84", "name": "Kitchen", "kind": "esp32-s3",
 "pair_code": "482913"}                     // first time
```

Hub:
1. `token` given: look up `token_hash`; the row's `id` must equal the
   hello's `id` (a token is bound to its device). OK → welcome with
   `"claimed": true, "owner": "Riya"`. Update `last_seen`, `last_fw`.
2. `pair_code` given (digits only, dash optional): check the hash and expiry
   and the rate limits. OK → create or replace the device row (owner = the
   code's user, name/kind from the hello), make a token (32 random bytes,
   base64url), store its hash, delete the code, then send
   `{"type": "claimed", "token": "...", "owner": "Riya"}` before the
   welcome. If the id already belongs to another user, the new code's user
   takes it (the person had the device in hand and a code) and the old
   owner's row goes.
3. Neither, or wrong: `{"type": "rejected", "reason": "pairing required"}`
   or `"bad token"` / `"bad code"`, close. Never say which part was wrong
   beyond that.
4. **Legacy tokens** (today's shared `hub_tokens` file): accepted only with
   `--legacy-tokens` (default on in M1 so nothing breaks, off in M2). A
   legacy edge is owned by the first admin and listed with `"legacy": true`
   so the dashboard can say "pair this device again". Log once per edge.

Old proto 2 edges send `token` only, which is case 1 or 4.

The token is a secret: never logged, never in a URL, compared by hash only.

## 6. Board firmware (waveshare)

- Settings: `POST /api/hub` takes `pair_code` (6 digits) next to `url`. The
  code is kept in RAM only until it is used or rejected; it is not stored.
- `HubLink` hello: when a pair code is set, hello carries `pair_code` and NOT
  the token (the pair code takes priority over any stored or legacy token; the
  hub checks pair_code first anyway). When no pair code is set, hello carries
  `token` if one is stored, else neither (the hub rejects: `last_error` says
  "pairing required" and the link waits for a settings change, as after any
  `rejected`).
- On `claimed`: save the new device token where the hub token is kept today
  (NVS, `credentials` component), drop the pair code, carry on (no reconnect
  needed).
- `GET /api/hub`: adds `paired` (bool), `owner` (name from the welcome),
  never the token.
- `rejected` with "bad code": keep the stored token unchanged (only the pair
  code is cleared) so a legacy board keeps working after a typo.
- `rejected` with "bad token" (device removed on the hub): clear the stored
  token so the person can pair again; log it.
- Standalone mode is untouched.
- Device dashboard (`web/apps/device`, Hub card): a "Pairing code" field and
  the `paired` / `owner` status.

## 7. Orbit

- Flag `--pair-code 482-913` (first run), token saved to
  `~/.config/nexus/orbit_token` (mode 600) on `claimed`, used afterwards.
  `--hub-token-file` keeps working for legacy tokens.

## 8. Hub web app (`web/apps/hub`)

Pages (React, the shared `@nexus/ui` pieces):
- `/setup`: first run: setup code, admin name, password.
- `/login`; forced password change when `must_change`.
- `/devices` (home): cards for the user's devices: name (editable), kind,
  online dot, firmware, uplink, last seen; remove (confirm); "Legacy:
  pair again" badge. **Add device**: shows the code large with a 10-minute
  countdown and short steps per kind (board: open the board's page → Hub →
  enter the hub address and the code; Orbit: the flag). The list refreshes
  live; when the new device appears, the dialog says "Paired: Kitchen".
- `/account`: display name, change password, sign out.
- `/admin/users` (admin): list, add (shows the temporary password once),
  reset, delete.
- A link per board to its own pages through the proxy
  (`/hub/d/{id}/`), serving `web/apps/device` with that API base.

## 9. Tests

Hub (`go test`, no network):
- store: migrations from empty, constraints, cascade on user delete.
- auth: hash/verify, wrong password, lock-out after 5, setup only once and
  only with the code, session expiry/sliding, logout, password change kills
  other sessions, CSRF (missing Origin, foreign Origin, wrong content type).
- pairing: code works once, expires, per-user limit, rate limit, token bound
  to id, wrong id rejected, re-pair moves ownership, legacy flag on/off.
- access: member can't see another's device, the proxy refuses a device the
  user doesn't own, `/hub/status` admin only.
- edge: proto 3 hello with code gets `claimed` then `welcome`; reconnect with
  the token works; deleted device's token is rejected.

Board: host test for the hello builder (token / pair_code / neither) if the
hello is built in portable code; otherwise on the device (section 11).

Web: builds; the device app bundle stays within ~5% of its size.

## 10. Security review before merging

- No secret in logs: grep the hub log after the tests for the token, the
  setup code, the pairing code, passwords.
- Tokens, codes and session ids only stored hashed; compared in constant
  time.
- Every `/hub/api` handler goes through one auth middleware; a test walks
  the router and fails for any route without it (except setup/login).
- No secret in URLs.

## 11. On the device (Claude)

1. Hub with `--legacy-tokens`: the board connects as today (legacy badge).
2. Create the admin with the setup code; create a second user.
3. Admin makes a code; enter it on the board; the board shows `paired`,
   owner = admin; reboot: it reconnects with its token.
4. Second user can't see the board or reach its API through the proxy.
5. Remove the board in the dashboard: it is rejected, clears its token,
   pairs again with a new code.
6. Orbit pairs with `--pair-code`.
7. `--legacy-tokens` off: the old token is rejected.
8. Voice session and music still work after pairing (regression).

## 12. Tasks (for AGY, in order, each reviewed before the next)

| Task | Repo | Contents |
|---|---|---|
| M1.1 | nexus | `pkg/store` + migrations, `pkg/auth` (passwords, sessions, CSRF, rate limits), setup/login/me/users endpoints, auth middleware on existing routes, tests |
| M1.2 | nexus | devices table use, pairing codes, proto 3 hello/claimed/rejected, legacy flag, device endpoints, `/hub/d/{id}/api` proxy, `PROTOCOL.md` §3.1 update, tests |
| M1.3 | nexus | `web/apps/hub`: setup, login, devices, add device, account, admin users; device app API base path + pairing field |
| M1.4 | waveshare | pair code setting, hello token/pair_code, `claimed` saves the token, `bad token` clears it, `/api/hub` fields, docs |
| M1.5 | nexus | Orbit `--pair-code` and its token file |

Standing rules for every task: local commits only, no push, no deploy, no
restarting the running hub or Orbit, no secrets printed or committed, the
board untouched by AGY (Claude flashes and tests).
