package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store manages persistence using SQLite.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// Open opens or creates an SQLite database at dbPath and executes pending migrations.
func Open(dbPath string) (*Store, error) {
	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("create store dir: %w", err)
			}
		}
	}

	dsn := dbPath
	if dbPath != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", dbPath)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// SQLite connection tuning
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Ensure pragmas are applied
	if _, err := db.Exec(`
		PRAGMA foreign_keys = ON;
		PRAGMA busy_timeout = 5000;
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if dbPath != ":memory:" {
		if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
			db.Close()
			return nil, fmt.Errorf("set WAL journal mode: %w", err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}

	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// DB returns the underlying sql.DB.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	type migration struct {
		version int
		name    string
	}
	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(entry.Name(), "_", 2)
		if len(parts) == 0 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid migration version prefix in %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: v, name: entry.Name()})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	for _, m := range migrations {
		var exists int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM schema_version WHERE version = ?`, m.version).Scan(&exists)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check schema version %d: %w", m.version, err)
		}

		sqlBytes, err := migrationFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("read migration file %s: %w", m.name, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration tx for %s: %w", m.name, err)
		}

		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("execute migration %s: %w", m.name, err)
		}

		now := time.Now().UTC().Unix()
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, m.version, now); err != nil {
			tx.Rollback()
			return fmt.Errorf("record schema version %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.name, err)
		}
	}

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Users
// ─────────────────────────────────────────────────────────────────────────────

// CountUsers returns the total number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

// CountAdmins returns the number of admin users.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&count)
	return count, err
}

// CreateUser inserts a new user record.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	if u.CreatedAt == 0 {
		u.CreatedAt = now
	}
	if u.UpdatedAt == 0 {
		u.UpdatedAt = now
	}

	mustChangeInt := 0
	if u.MustChange {
		mustChangeInt = 1
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, display_name, role, pass_hash, must_change, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, u.Username, u.DisplayName, u.Role, u.PassHash, mustChangeInt, u.CreatedAt, u.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("insert user: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("user last insert id: %w", err)
	}
	u.ID = id
	return nil
}

// GetUserByID fetches a user by primary key.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u User
	var mustChangeInt int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, pass_hash, must_change, created_at, updated_at
		FROM users WHERE id = ?
	`, id).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.PassHash, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	u.MustChange = (mustChangeInt != 0)
	return &u, nil
}

// GetUserByUsername fetches a user by case-insensitive username.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u User
	var mustChangeInt int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, pass_hash, must_change, created_at, updated_at
		FROM users WHERE username = ?
	`, username).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.PassHash, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	u.MustChange = (mustChangeInt != 0)
	return &u, nil
}

// ListUsers returns all users sorted by ID.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, username, display_name, role, must_change, created_at, updated_at
		FROM users ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list users query: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		var u User
		var mustChangeInt int
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.MustChange = (mustChangeInt != 0)
		users = append(users, &u)
	}
	return users, rows.Err()
}

// UpdateUserPassword updates the password hash and must_change flag for a user.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passHash string, mustChange bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	mustChangeInt := 0
	if mustChange {
		mustChangeInt = 1
	}
	now := time.Now().UTC().Unix()

	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET pass_hash = ?, must_change = ?, updated_at = ? WHERE id = ?
	`, passHash, mustChangeInt, now, id)
	if err != nil {
		return fmt.Errorf("update user password: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserDisplayName updates the display name for a user.
func (s *Store) UpdateUserDisplayName(ctx context.Context, id int64, displayName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET display_name = ?, updated_at = ? WHERE id = ?
	`, displayName, now, id)
	if err != nil {
		return fmt.Errorf("update user display name: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser deletes a user and cascades to associated sessions and devices.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Sessions
// ─────────────────────────────────────────────────────────────────────────────

// CreateSession stores a new active session.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	if sess.CreatedAt == 0 {
		sess.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user_id, created_at, expires_at, user_agent, ip)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sess.IDHash[:], sess.UserID, sess.CreatedAt, sess.ExpiresAt, sess.UserAgent, sess.IP)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSession retrieves an active session and the corresponding user by token sha256.
func (s *Store) GetSession(ctx context.Context, idHash [32]byte) (*Session, *User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UTC().Unix()
	var sess Session
	var u User
	var hashBytes []byte
	var mustChangeInt int
	var ua sql.NullString
	var ip sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT s.id_hash, s.user_id, s.created_at, s.expires_at, s.user_agent, s.ip,
		       u.id, u.username, u.display_name, u.role, u.pass_hash, u.must_change, u.created_at, u.updated_at
		FROM sessions s
		JOIN users u ON s.user_id = u.id
		WHERE s.id_hash = ? AND s.expires_at > ?
	`, idHash[:], now).Scan(
		&hashBytes, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &ua, &ip,
		&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.PassHash, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("get session: %w", err)
	}

	copy(sess.IDHash[:], hashBytes)
	sess.UserAgent = ua.String
	sess.IP = ip.String
	u.MustChange = (mustChangeInt != 0)

	return &sess, &u, nil
}

// TouchSession extends the session's sliding expiration.
func (s *Store) TouchSession(ctx context.Context, idHash [32]byte, newExpiresAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET expires_at = ? WHERE id_hash = ?
	`, newExpiresAt, idHash[:])
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSession deletes a specific session by token sha256.
func (s *Store) DeleteSession(ctx context.Context, idHash [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash[:])
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSessionsForUserExcept deletes all sessions for a user, optionally preserving one session.
func (s *Store) DeleteSessionsForUserExcept(ctx context.Context, userID int64, exceptHash *[32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if exceptHash != nil {
		_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash != ?`, userID, exceptHash[:])
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// DeleteExpiredSessions removes all expired sessions from the database.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now)
	return err
}
