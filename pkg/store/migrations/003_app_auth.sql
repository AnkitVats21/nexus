-- 003_app_auth.sql
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending','active','disabled'));

CREATE TABLE refresh_tokens (
  id_hash     BLOB PRIMARY KEY,        -- sha256 of the token
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id   TEXT REFERENCES devices(id) ON DELETE SET NULL,
  family_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER,                 -- NULL until rotated
  user_agent  TEXT,
  ip          TEXT
);

CREATE INDEX idx_refresh_tokens_family ON refresh_tokens(family_id);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens(user_id);
