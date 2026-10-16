-- 002_push_tokens.sql
CREATE TABLE push_tokens (
  device_id   TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
  provider    TEXT NOT NULL CHECK (provider IN ('fcm')),
  token       TEXT NOT NULL,         -- FCM registration token
  updated_at  INTEGER NOT NULL,
  last_ok_at  INTEGER,               -- last successful send
  fail_count  INTEGER NOT NULL DEFAULT 0
);
