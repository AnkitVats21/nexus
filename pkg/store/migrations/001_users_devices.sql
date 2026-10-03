-- 001_users_devices.sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
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
  user_agent  TEXT,
  ip          TEXT
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
