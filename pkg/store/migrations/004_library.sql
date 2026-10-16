-- 004_library.sql
CREATE TABLE tracks (
  video_id    TEXT PRIMARY KEY,
  title       TEXT NOT NULL,
  artist      TEXT NOT NULL,
  album       TEXT,
  duration_s  INTEGER NOT NULL,
  thumb_url   TEXT,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE likes (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  video_id    TEXT NOT NULL REFERENCES tracks(video_id),
  liked_at    INTEGER NOT NULL,
  PRIMARY KEY (user_id, video_id)
);
CREATE INDEX idx_likes_user_liked_at ON likes(user_id, liked_at DESC);

CREATE TABLE playlists (
  id          TEXT PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_playlists_user_updated ON playlists(user_id, updated_at DESC);

CREATE TABLE playlist_items (
  item_id     TEXT PRIMARY KEY,
  playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  video_id    TEXT NOT NULL REFERENCES tracks(video_id),
  added_at    INTEGER NOT NULL
);
CREATE INDEX idx_playlist_items_pos ON playlist_items(playlist_id, position);

CREATE TABLE history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  video_id    TEXT NOT NULL REFERENCES tracks(video_id),
  played_at   INTEGER NOT NULL,
  played_ms   INTEGER NOT NULL,
  device_id   TEXT REFERENCES devices(id) ON DELETE SET NULL,
  source      TEXT NOT NULL CHECK (source IN ('app','board','dashboard')),
  UNIQUE (user_id, video_id, played_at)
);
CREATE INDEX idx_history_user_played_at ON history(user_id, played_at DESC);

CREATE TABLE library_versions (
  user_id           INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  likes_version     INTEGER NOT NULL DEFAULT 0,
  playlists_version INTEGER NOT NULL DEFAULT 0,
  history_version   INTEGER NOT NULL DEFAULT 0
);
