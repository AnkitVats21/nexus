package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
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

// CountActiveAdmins returns the number of active admin users.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'`).Scan(&count)
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
	if u.Status == "" {
		u.Status = "active"
	}

	mustChangeInt := 0
	if u.MustChange {
		mustChangeInt = 1
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, display_name, role, status, pass_hash, must_change, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, u.Username, u.DisplayName, u.Role, u.Status, u.PassHash, mustChangeInt, u.CreatedAt, u.UpdatedAt)
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
		SELECT id, username, display_name, role, status, pass_hash, must_change, created_at, updated_at
		FROM users WHERE id = ?
	`, id).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.PassHash, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt)
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
		SELECT id, username, display_name, role, status, pass_hash, must_change, created_at, updated_at
		FROM users WHERE username = ?
	`, username).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.PassHash, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	u.MustChange = (mustChangeInt != 0)
	return &u, nil
}

// ListUsers returns all users, optionally filtered by status (if specified).
func (s *Store) ListUsers(ctx context.Context, status ...string) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, username, display_name, role, status, must_change, created_at, updated_at
		FROM users
	`
	var args []any
	if len(status) > 0 && status[0] != "" {
		query += ` WHERE status = ?`
		args = append(args, status[0])
	}
	query += ` ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list users query: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		var u User
		var mustChangeInt int
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.MustChange = (mustChangeInt != 0)
		users = append(users, &u)
	}
	return users, rows.Err()
}

// UpdateUserStatus updates the status of a user ('pending', 'active', 'disabled').
func (s *Store) UpdateUserStatus(ctx context.Context, id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET status = ?, updated_at = ? WHERE id = ?
	`, status, now, id)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
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
		       u.id, u.username, u.display_name, u.role, u.status, u.pass_hash, u.must_change, u.created_at, u.updated_at
		FROM sessions s
		JOIN users u ON s.user_id = u.id
		WHERE s.id_hash = ? AND s.expires_at > ?
	`, idHash[:], now).Scan(
		&hashBytes, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &ua, &ip,
		&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.PassHash, &mustChangeInt, &u.CreatedAt, &u.UpdatedAt,
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

// --- Devices ---

// UpsertDevice inserts a new device or updates an existing device if its ID already exists (e.g. re-pairing).
func (s *Store) UpsertDevice(ctx context.Context, d *Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (id, owner_id, name, kind, token_hash, created_at, last_seen, last_fw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			owner_id = excluded.owner_id,
			name = excluded.name,
			kind = excluded.kind,
			token_hash = excluded.token_hash,
			last_seen = excluded.last_seen,
			last_fw = excluded.last_fw
	`, d.ID, d.OwnerID, d.Name, d.Kind, d.TokenHash[:], d.CreatedAt, d.LastSeen, d.LastFW)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

// GetDeviceByID retrieves a device by its ID.
func (s *Store) GetDeviceByID(ctx context.Context, id string) (*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var d Device
	var tokenHash []byte
	var lastSeen sql.NullInt64
	var lastFW sql.NullString

	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, name, kind, token_hash, created_at, last_seen, last_fw
		FROM devices
		WHERE id = ?
	`, id)
	err := row.Scan(&d.ID, &d.OwnerID, &d.Name, &d.Kind, &tokenHash, &d.CreatedAt, &lastSeen, &lastFW)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get device by id: %w", err)
	}
	copy(d.TokenHash[:], tokenHash)
	if lastSeen.Valid {
		d.LastSeen = lastSeen.Int64
	}
	if lastFW.Valid {
		d.LastFW = lastFW.String
	}
	return &d, nil
}

// GetDeviceByTokenHash retrieves a device by sha256 token hash.
func (s *Store) GetDeviceByTokenHash(ctx context.Context, tokenHash [32]byte) (*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var d Device
	var hashBytes []byte
	var lastSeen sql.NullInt64
	var lastFW sql.NullString

	row := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, name, kind, token_hash, created_at, last_seen, last_fw
		FROM devices
		WHERE token_hash = ?
	`, tokenHash[:])
	err := row.Scan(&d.ID, &d.OwnerID, &d.Name, &d.Kind, &hashBytes, &d.CreatedAt, &lastSeen, &lastFW)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get device by token hash: %w", err)
	}
	copy(d.TokenHash[:], hashBytes)
	if lastSeen.Valid {
		d.LastSeen = lastSeen.Int64
	}
	if lastFW.Valid {
		d.LastFW = lastFW.String
	}
	return &d, nil
}

// ListDevicesForUser lists all devices owned by a specific user.
func (s *Store) ListDevicesForUser(ctx context.Context, userID int64) ([]*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_id, name, kind, token_hash, created_at, last_seen, last_fw
		FROM devices
		WHERE owner_id = ?
		ORDER BY created_at ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices for user: %w", err)
	}
	defer rows.Close()

	var devices []*Device
	for rows.Next() {
		var d Device
		var hashBytes []byte
		var lastSeen sql.NullInt64
		var lastFW sql.NullString
		if err := rows.Scan(&d.ID, &d.OwnerID, &d.Name, &d.Kind, &hashBytes, &d.CreatedAt, &lastSeen, &lastFW); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		copy(d.TokenHash[:], hashBytes)
		if lastSeen.Valid {
			d.LastSeen = lastSeen.Int64
		}
		if lastFW.Valid {
			d.LastFW = lastFW.String
		}
		devices = append(devices, &d)
	}
	return devices, rows.Err()
}

// ListAllDevices lists all devices across all users (for admin view).
func (s *Store) ListAllDevices(ctx context.Context) ([]*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_id, name, kind, token_hash, created_at, last_seen, last_fw
		FROM devices
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list all devices: %w", err)
	}
	defer rows.Close()

	var devices []*Device
	for rows.Next() {
		var d Device
		var hashBytes []byte
		var lastSeen sql.NullInt64
		var lastFW sql.NullString
		if err := rows.Scan(&d.ID, &d.OwnerID, &d.Name, &d.Kind, &hashBytes, &d.CreatedAt, &lastSeen, &lastFW); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		copy(d.TokenHash[:], hashBytes)
		if lastSeen.Valid {
			d.LastSeen = lastSeen.Int64
		}
		if lastFW.Valid {
			d.LastFW = lastFW.String
		}
		devices = append(devices, &d)
	}
	return devices, rows.Err()
}

// UpdateDeviceName updates the friendly name of a device.
func (s *Store) UpdateDeviceName(ctx context.Context, id string, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("update device name: %w", err)
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

// UpdateDeviceLastSeen updates the last_seen timestamp and last_fw for a device.
func (s *Store) UpdateDeviceLastSeen(ctx context.Context, id string, lastSeen int64, fw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen = ?, last_fw = ? WHERE id = ?`, lastSeen, fw, id)
	return err
}

// DeleteDevice deletes a device by its ID.
func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
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

// --- Pairing Codes ---

// CreatePairingCode stores a new one-time pairing code.
func (s *Store) CreatePairingCode(ctx context.Context, pc *PairingCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	if pc.CreatedAt == 0 {
		pc.CreatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pairing_codes (code_hash, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)
	`, pc.CodeHash[:], pc.UserID, pc.ExpiresAt, pc.CreatedAt)
	if err != nil {
		return fmt.Errorf("create pairing code: %w", err)
	}
	return nil
}

// GetPairingCode retrieves a pairing code by its code hash.
func (s *Store) GetPairingCode(ctx context.Context, codeHash [32]byte) (*PairingCode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pc PairingCode
	var hashBytes []byte
	row := s.db.QueryRowContext(ctx, `
		SELECT code_hash, user_id, expires_at, created_at
		FROM pairing_codes
		WHERE code_hash = ?
	`, codeHash[:])
	err := row.Scan(&hashBytes, &pc.UserID, &pc.ExpiresAt, &pc.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get pairing code: %w", err)
	}
	copy(pc.CodeHash[:], hashBytes)
	return &pc, nil
}

// CountActivePairingCodes counts non-expired pairing codes for a specific user.
func (s *Store) CountActivePairingCodes(ctx context.Context, userID int64, now int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pairing_codes WHERE user_id = ? AND expires_at > ?
	`, userID, now).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active pairing codes: %w", err)
	}
	return count, nil
}

// ListActivePairingCodes returns all non-expired pairing codes for a specific user.
func (s *Store) ListActivePairingCodes(ctx context.Context, userID int64, now int64) ([]*PairingCode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT code_hash, user_id, expires_at, created_at
		FROM pairing_codes
		WHERE user_id = ? AND expires_at > ?
		ORDER BY created_at ASC
	`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list active pairing codes: %w", err)
	}
	defer rows.Close()

	var codes []*PairingCode
	for rows.Next() {
		var pc PairingCode
		var hashBytes []byte
		if err := rows.Scan(&hashBytes, &pc.UserID, &pc.ExpiresAt, &pc.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pairing code: %w", err)
		}
		copy(pc.CodeHash[:], hashBytes)
		codes = append(codes, &pc)
	}
	return codes, rows.Err()
}

// DeletePairingCode deletes a pairing code by its hash.
func (s *Store) DeletePairingCode(ctx context.Context, codeHash [32]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = ?`, codeHash[:])
	if err != nil {
		return fmt.Errorf("delete pairing code: %w", err)
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

// DeletePairingCodesForUser deletes all pairing codes for a specific user.
func (s *Store) DeletePairingCodesForUser(ctx context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE user_id = ?`, userID)
	return err
}

// DeleteExpiredPairingCodes deletes all expired pairing codes.
func (s *Store) DeleteExpiredPairingCodes(ctx context.Context, now int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE expires_at <= ?`, now)
	return err
}

// ClaimPairingCode atomically consumes a pairing code, upserts the device,
// and returns the owner user. If the code is missing or expired, it returns ErrNotFound.
func (s *Store) ClaimPairingCode(ctx context.Context, codeHash [32]byte, now int64, d *Device) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var userID int64
	err = tx.QueryRowContext(ctx, `
		SELECT user_id FROM pairing_codes WHERE code_hash = ? AND expires_at > ?
	`, codeHash[:], now).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup pairing code: %w", err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_hash = ?`, codeHash[:])
	if err != nil {
		return nil, fmt.Errorf("delete pairing code: %w", err)
	}

	var u User
	var mustChange int
	err = tx.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, pass_hash, must_change, created_at, updated_at
		FROM users
		WHERE id = ?
	`, userID).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.PassHash, &mustChange, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup user: %w", err)
	}
	u.MustChange = (mustChange != 0)

	d.OwnerID = userID
	if d.CreatedAt == 0 {
		d.CreatedAt = now
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO devices (id, owner_id, name, kind, token_hash, created_at, last_seen, last_fw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			owner_id = excluded.owner_id,
			name = excluded.name,
			kind = excluded.kind,
			token_hash = excluded.token_hash,
			last_seen = excluded.last_seen,
			last_fw = excluded.last_fw
	`, d.ID, d.OwnerID, d.Name, d.Kind, d.TokenHash[:], d.CreatedAt, d.LastSeen, d.LastFW)
	if err != nil {
		return nil, fmt.Errorf("upsert device in tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}

	return &u, nil
}

// GetFirstAdmin returns the first created admin user (for legacy device ownership).
func (s *Store) GetFirstAdmin(ctx context.Context) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u User
	var mustChange int
	row := s.db.QueryRowContext(ctx, `
		SELECT id, username, display_name, role, pass_hash, must_change, created_at, updated_at
		FROM users
		WHERE role = 'admin'
		ORDER BY id ASC
		LIMIT 1
	`)
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.PassHash, &mustChange, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get first admin: %w", err)
	}
	u.MustChange = (mustChange != 0)
	return &u, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Push Tokens
// ─────────────────────────────────────────────────────────────────────────────

// UpsertPushToken stores or updates an FCM push token for a device.
func (s *Store) UpsertPushToken(ctx context.Context, pt *PushToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	if pt.UpdatedAt == 0 {
		pt.UpdatedAt = now
	}

	var lastOK any = nil
	if pt.LastOKAt > 0 {
		lastOK = pt.LastOKAt
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO push_tokens (device_id, provider, token, updated_at, last_ok_at, fail_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			provider = excluded.provider,
			token = excluded.token,
			updated_at = excluded.updated_at,
			fail_count = 0
	`, pt.DeviceID, pt.Provider, pt.Token, pt.UpdatedAt, lastOK, pt.FailCount)
	if err != nil {
		return fmt.Errorf("upsert push token: %w", err)
	}
	return nil
}

// DeletePushToken removes the push token for a device.
func (s *Store) DeletePushToken(ctx context.Context, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM push_tokens WHERE device_id = ?`, deviceID)
	if err != nil {
		return fmt.Errorf("delete push token: %w", err)
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

// GetPushToken retrieves the push token for a device.
func (s *Store) GetPushToken(ctx context.Context, deviceID string) (*PushToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pt PushToken
	var lastOK sql.NullInt64
	row := s.db.QueryRowContext(ctx, `
		SELECT device_id, provider, token, updated_at, last_ok_at, fail_count
		FROM push_tokens
		WHERE device_id = ?
	`, deviceID)
	err := row.Scan(&pt.DeviceID, &pt.Provider, &pt.Token, &pt.UpdatedAt, &lastOK, &pt.FailCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get push token: %w", err)
	}
	if lastOK.Valid {
		pt.LastOKAt = lastOK.Int64
	}
	return &pt, nil
}

// MarkPushOK records a successful push send to the device, resetting fail_count.
func (s *Store) MarkPushOK(ctx context.Context, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE push_tokens
		SET last_ok_at = ?, fail_count = 0
		WHERE device_id = ?
	`, now, deviceID)
	if err != nil {
		return fmt.Errorf("mark push ok: %w", err)
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

// MarkPushFail records a failed push attempt to the device, incrementing fail_count.
func (s *Store) MarkPushFail(ctx context.Context, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE push_tokens
		SET fail_count = fail_count + 1
		WHERE device_id = ?
	`, deviceID)
	if err != nil {
		return fmt.Errorf("mark push fail: %w", err)
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

// DeletePushTokenIf removes the push token for a device only if the current token matches the expected token.
func (s *Store) DeletePushTokenIf(ctx context.Context, deviceID string, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM push_tokens WHERE device_id = ? AND token = ?`, deviceID, token)
	if err != nil {
		return fmt.Errorf("delete push token if matching: %w", err)
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

// MarkPushOKIf records a successful push send only if the token has not changed since the send started.
func (s *Store) MarkPushOKIf(ctx context.Context, deviceID string, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE push_tokens
		SET last_ok_at = ?, fail_count = 0
		WHERE device_id = ? AND token = ?
	`, now, deviceID, token)
	if err != nil {
		return fmt.Errorf("mark push ok if matching: %w", err)
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

// MarkPushFailIf records a failed push attempt only if the token has not changed since the send started.
func (s *Store) MarkPushFailIf(ctx context.Context, deviceID string, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE push_tokens
		SET fail_count = fail_count + 1
		WHERE device_id = ? AND token = ?
	`, deviceID, token)
	if err != nil {
		return fmt.Errorf("mark push fail if matching: %w", err)
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
// Refresh Tokens
// ─────────────────────────────────────────────────────────────────────────────

// CreateRefreshToken inserts a new refresh token record.
func (s *Store) CreateRefreshToken(ctx context.Context, rt *RefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Unix()
	if rt.CreatedAt == 0 {
		rt.CreatedAt = now
	}

	var devID any
	if rt.DeviceID != nil && *rt.DeviceID != "" {
		devID = *rt.DeviceID
	}

	var usedAt any
	if rt.UsedAt != nil {
		usedAt = *rt.UsedAt
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO refresh_tokens (id_hash, user_id, device_id, family_id, created_at, expires_at, used_at, user_agent, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rt.IDHash[:], rt.UserID, devID, rt.FamilyID, rt.CreatedAt, rt.ExpiresAt, usedAt, rt.UserAgent, rt.IP)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrConflict
		}
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

// GetRefreshTokenByHash retrieves a refresh token by its sha256 hash.
func (s *Store) GetRefreshTokenByHash(ctx context.Context, hash [32]byte) (*RefreshToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var rt RefreshToken
	var hashBytes []byte
	var devID sql.NullString
	var usedAt sql.NullInt64
	var userAgent, ip sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id_hash, user_id, device_id, family_id, created_at, expires_at, used_at, user_agent, ip
		FROM refresh_tokens WHERE id_hash = ?
	`, hash[:]).Scan(&hashBytes, &rt.UserID, &devID, &rt.FamilyID, &rt.CreatedAt, &rt.ExpiresAt, &usedAt, &userAgent, &ip)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get refresh token: %w", err)
	}

	copy(rt.IDHash[:], hashBytes)

	if devID.Valid {
		v := devID.String
		rt.DeviceID = &v
	}
	if usedAt.Valid {
		v := usedAt.Int64
		rt.UsedAt = &v
	}
	if userAgent.Valid {
		rt.UserAgent = userAgent.String
	}
	if ip.Valid {
		rt.IP = ip.String
	}
	return &rt, nil
}

// MarkRefreshTokenUsed marks a refresh token as rotated with the given timestamp.
func (s *Store) MarkRefreshTokenUsed(ctx context.Context, hash [32]byte, usedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE refresh_tokens SET used_at = ? WHERE id_hash = ? AND used_at IS NULL
	`, usedAt, hash[:])
	if err != nil {
		return fmt.Errorf("mark refresh token used: %w", err)
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

// RotateRefreshToken atomically marks oldHash as used and inserts newRT in the same family.
// If oldHash was already used (concurrent rotation or replay), it invalidates the entire family and returns ErrNotFound.
func (s *Store) RotateRefreshToken(ctx context.Context, oldHash [32]byte, usedAt int64, newRT *RefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE refresh_tokens SET used_at = ? WHERE id_hash = ? AND used_at IS NULL
	`, usedAt, oldHash[:])
	if err != nil {
		return fmt.Errorf("mark refresh token used: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE family_id = ?`, newRT.FamilyID)
		return ErrNotFound
	}

	var devID any
	if newRT.DeviceID != nil && *newRT.DeviceID != "" {
		devID = *newRT.DeviceID
	}
	var userAgent any
	if newRT.UserAgent != "" {
		userAgent = newRT.UserAgent
	}
	var ip any
	if newRT.IP != "" {
		ip = newRT.IP
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO refresh_tokens (id_hash, user_id, device_id, family_id, created_at, expires_at, used_at, user_agent, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, newRT.IDHash[:], newRT.UserID, devID, newRT.FamilyID, newRT.CreatedAt, newRT.ExpiresAt, newRT.UsedAt, userAgent, ip)
	if err != nil {
		return fmt.Errorf("create rotated refresh token: %w", err)
	}
	return nil
}

// DeleteRefreshTokensByFamily removes all tokens in a family (e.g. on logout or replay detection).
func (s *Store) DeleteRefreshTokensByFamily(ctx context.Context, familyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE family_id = ?`, familyID)
	if err != nil {
		return fmt.Errorf("delete refresh tokens by family: %w", err)
	}
	return nil
}

// DeleteRefreshTokensForUser removes all refresh tokens for a user (e.g. on disable or password change).
func (s *Store) DeleteRefreshTokensForUser(ctx context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete refresh tokens for user: %w", err)
	}
	return nil
}

// BindRefreshFamilyToDevice binds a refresh family to a device ID.
func (s *Store) BindRefreshFamilyToDevice(ctx context.Context, familyID string, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE refresh_tokens SET device_id = ? WHERE family_id = ?
	`, deviceID, familyID)
	if err != nil {
		return fmt.Errorf("bind refresh family to device: %w", err)
	}
	return nil
}

// PruneRefreshTokens deletes rows with expires_at < now.
// Used rows that have not yet expired are preserved to allow reuse detection.
func (s *Store) PruneRefreshTokens(ctx context.Context, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE expires_at < ?`, now)
	if err != nil {
		return 0, fmt.Errorf("prune refresh tokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune refresh tokens rows affected: %w", err)
	}
	return n, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Music Library (tracks, likes, playlists, history, library_versions)
// ─────────────────────────────────────────────────────────────────────────────

func randomURLSafeID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func upsertTrackTx(ctx context.Context, tx *sql.Tx, t Track, now int64) error {
	if t.ID == "" {
		return errors.New("empty track id")
	}
	if now == 0 {
		now = time.Now().UTC().Unix()
	}
	var album any
	if t.Album != "" {
		album = t.Album
	}
	var thumb any
	if t.Thumbnail != "" {
		thumb = t.Thumbnail
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO tracks (video_id, title, artist, album, duration_s, thumb_url, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(video_id) DO UPDATE SET
			title = excluded.title,
			artist = excluded.artist,
			album = excluded.album,
			duration_s = excluded.duration_s,
			thumb_url = excluded.thumb_url,
			updated_at = excluded.updated_at
	`, t.ID, t.Title, t.Artist, album, t.Duration, thumb, now)
	if err != nil {
		return fmt.Errorf("upsert track %s: %w", t.ID, err)
	}
	return nil
}

// UpsertTrack inserts or updates a track metadata snapshot.
func (s *Store) UpsertTrack(ctx context.Context, t *Track) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := upsertTrackTx(ctx, tx, *t, time.Now().UTC().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// GetLibraryVersions returns the current sync versions for a user.
func (s *Store) GetLibraryVersions(ctx context.Context, userID int64) (LibraryVersions, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var lv LibraryVersions
	lv.UserID = userID
	row := s.db.QueryRowContext(ctx, `
		SELECT likes_version, playlists_version, history_version
		FROM library_versions WHERE user_id = ?
	`, userID)
	if err := row.Scan(&lv.LikesVersion, &lv.PlaylistsVersion, &lv.HistoryVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lv, nil
		}
		return lv, fmt.Errorf("get library versions: %w", err)
	}
	return lv, nil
}

func bumpLikesVersionTx(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO library_versions (user_id, likes_version, playlists_version, history_version)
		VALUES (?, 1, 0, 0)
		ON CONFLICT(user_id) DO UPDATE SET likes_version = likes_version + 1
	`, userID)
	if err != nil {
		return 0, fmt.Errorf("bump likes_version: %w", err)
	}
	var newVer int64
	if err := tx.QueryRowContext(ctx, `SELECT likes_version FROM library_versions WHERE user_id = ?`, userID).Scan(&newVer); err != nil {
		return 0, fmt.Errorf("query bumped likes_version: %w", err)
	}
	return newVer, nil
}

func bumpPlaylistsVersionTx(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO library_versions (user_id, likes_version, playlists_version, history_version)
		VALUES (?, 0, 1, 0)
		ON CONFLICT(user_id) DO UPDATE SET playlists_version = playlists_version + 1
	`, userID)
	if err != nil {
		return 0, fmt.Errorf("bump playlists_version: %w", err)
	}
	var newVer int64
	if err := tx.QueryRowContext(ctx, `SELECT playlists_version FROM library_versions WHERE user_id = ?`, userID).Scan(&newVer); err != nil {
		return 0, fmt.Errorf("query bumped playlists_version: %w", err)
	}
	return newVer, nil
}

func bumpHistoryVersionTx(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO library_versions (user_id, likes_version, playlists_version, history_version)
		VALUES (?, 0, 0, 1)
		ON CONFLICT(user_id) DO UPDATE SET history_version = history_version + 1
	`, userID)
	if err != nil {
		return 0, fmt.Errorf("bump history_version: %w", err)
	}
	var newVer int64
	if err := tx.QueryRowContext(ctx, `SELECT history_version FROM library_versions WHERE user_id = ?`, userID).Scan(&newVer); err != nil {
		return 0, fmt.Errorf("query bumped history_version: %w", err)
	}
	return newVer, nil
}

// PutLike adds a song to user's likes. Idempotent: preserves original liked_at, upserts track, bumps likes_version.
func (s *Store) PutLike(ctx context.Context, userID int64, t Track, likedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if likedAt == 0 {
		likedAt = time.Now().UTC().UnixMilli()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := upsertTrackTx(ctx, tx, t, time.Now().UTC().Unix()); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO likes (user_id, video_id, liked_at)
		VALUES (?, ?, ?)
		ON CONFLICT(user_id, video_id) DO NOTHING
	`, userID, t.ID, likedAt)
	if err != nil {
		return fmt.Errorf("insert like: %w", err)
	}

	if _, err := bumpLikesVersionTx(ctx, tx, userID); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteLike removes a song from user's likes. Idempotent: bumps likes_version.
func (s *Store) DeleteLike(ctx context.Context, userID int64, videoID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM likes WHERE user_id = ? AND video_id = ?`, userID, videoID)
	if err != nil {
		return fmt.Errorf("delete like: %w", err)
	}

	if _, err := bumpLikesVersionTx(ctx, tx, userID); err != nil {
		return err
	}

	return tx.Commit()
}

// GetLikes returns liked tracks newest first with pagination.
func (s *Store) GetLikes(ctx context.Context, userID int64, limit, offset int) ([]Like, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.user_id, l.video_id, l.liked_at,
		       t.title, t.artist, t.album, t.duration_s, t.thumb_url, t.updated_at
		FROM likes l
		JOIN tracks t ON l.video_id = t.video_id
		WHERE l.user_id = ?
		ORDER BY l.liked_at DESC
		LIMIT ? OFFSET ?
	`, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query likes: %w", err)
	}
	defer rows.Close()

	var out []Like
	for rows.Next() {
		var l Like
		var album, thumb sql.NullString
		if err := rows.Scan(
			&l.UserID, &l.VideoID, &l.LikedAt,
			&l.Track.Title, &l.Track.Artist, &album, &l.Track.Duration, &thumb, &l.Track.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan like: %w", err)
		}
		l.Track.ID = l.VideoID
		if album.Valid {
			l.Track.Album = album.String
		}
		if thumb.Valid {
			l.Track.Thumbnail = thumb.String
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetLikedIDs returns all liked video IDs for a user (newest first).
func (s *Store) GetLikedIDs(ctx context.Context, userID int64) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT video_id FROM likes WHERE user_id = ? ORDER BY liked_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("query liked ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan liked id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CreatePlaylist creates a new playlist for the user. Enforces 500 playlists limit.
func (s *Store) CreatePlaylist(ctx context.Context, p *Playlist) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlists WHERE user_id = ?`, p.UserID).Scan(&count); err != nil {
		return fmt.Errorf("count playlists: %w", err)
	}
	if count >= 500 {
		return ErrLimitExceeded
	}

	if p.ID == "" {
		p.ID = randomURLSafeID(16)
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().UTC().Unix()
	}
	p.UpdatedAt = p.CreatedAt
	p.Version = 1

	_, err = tx.ExecContext(ctx, `
		INSERT INTO playlists (id, user_id, name, created_at, updated_at, version)
		VALUES (?, ?, ?, ?, ?, ?)
	`, p.ID, p.UserID, p.Name, p.CreatedAt, p.UpdatedAt, p.Version)
	if err != nil {
		return fmt.Errorf("insert playlist: %w", err)
	}

	if _, err := bumpPlaylistsVersionTx(ctx, tx, p.UserID); err != nil {
		return err
	}

	return tx.Commit()
}

// GetPlaylists returns all playlists for a user with count and up to 4 thumbnails.
func (s *Store) GetPlaylists(ctx context.Context, userID int64) ([]Playlist, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.user_id, p.name, p.created_at, p.updated_at, p.version, COUNT(pi.item_id)
		FROM playlists p
		LEFT JOIN playlist_items pi ON p.id = pi.playlist_id
		WHERE p.user_id = ?
		GROUP BY p.id
		ORDER BY p.updated_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("query playlists: %w", err)
	}
	defer rows.Close()

	var playlists []Playlist
	playlistMap := make(map[string]int) // id -> index in playlists
	for rows.Next() {
		var pl Playlist
		if err := rows.Scan(&pl.ID, &pl.UserID, &pl.Name, &pl.CreatedAt, &pl.UpdatedAt, &pl.Version, &pl.Count); err != nil {
			return nil, fmt.Errorf("scan playlist: %w", err)
		}
		pl.Thumbnails = []string{}
		playlistMap[pl.ID] = len(playlists)
		playlists = append(playlists, pl)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(playlists) == 0 {
		return playlists, nil
	}

	// Fetch up to 4 thumbnails per playlist in order of position ASC
	thumbRows, err := s.db.QueryContext(ctx, `
		SELECT pi.playlist_id, t.thumb_url
		FROM playlist_items pi
		JOIN tracks t ON pi.video_id = t.video_id
		JOIN playlists p ON pi.playlist_id = p.id
		WHERE p.user_id = ? AND t.thumb_url IS NOT NULL AND t.thumb_url != ''
		ORDER BY pi.playlist_id, pi.position ASC
	`, userID)
	if err == nil {
		defer thumbRows.Close()
		for thumbRows.Next() {
			var pid, thumb string
			if err := thumbRows.Scan(&pid, &thumb); err == nil {
				if idx, ok := playlistMap[pid]; ok {
					if len(playlists[idx].Thumbnails) < 4 {
						playlists[idx].Thumbnails = append(playlists[idx].Thumbnails, thumb)
					}
				}
			}
		}
	}

	return playlists, nil
}

// GetPlaylist returns a playlist by ID if owned by userID, else ErrNotFound.
func (s *Store) GetPlaylist(ctx context.Context, userID int64, playlistID string) (*Playlist, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pl Playlist
	row := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.user_id, p.name, p.created_at, p.updated_at, p.version, COUNT(pi.item_id)
		FROM playlists p
		LEFT JOIN playlist_items pi ON p.id = pi.playlist_id
		WHERE p.id = ? AND p.user_id = ?
		GROUP BY p.id
	`, playlistID, userID)
	if err := row.Scan(&pl.ID, &pl.UserID, &pl.Name, &pl.CreatedAt, &pl.UpdatedAt, &pl.Version, &pl.Count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get playlist: %w", err)
	}

	pl.Thumbnails = []string{}
	thumbRows, err := s.db.QueryContext(ctx, `
		SELECT t.thumb_url
		FROM playlist_items pi
		JOIN tracks t ON pi.video_id = t.video_id
		WHERE pi.playlist_id = ? AND t.thumb_url IS NOT NULL AND t.thumb_url != ''
		ORDER BY pi.position ASC
		LIMIT 4
	`, playlistID)
	if err == nil {
		defer thumbRows.Close()
		for thumbRows.Next() {
			var thumb string
			if err := thumbRows.Scan(&thumb); err == nil {
				pl.Thumbnails = append(pl.Thumbnails, thumb)
			}
		}
	}

	return &pl, nil
}

// GetPlaylistItems returns playlist items in position order with pagination and total count.
func (s *Store) GetPlaylistItems(ctx context.Context, userID int64, playlistID string, limit, offset int) ([]PlaylistItem, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Verify ownership
	var curVer int64
	err := s.db.QueryRowContext(ctx, `SELECT version FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&curVer)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("check playlist ownership: %w", err)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ?`, playlistID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count playlist items: %w", err)
	}

	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT pi.item_id, pi.playlist_id, pi.position, pi.video_id, pi.added_at,
		       t.title, t.artist, t.album, t.duration_s, t.thumb_url, t.updated_at
		FROM playlist_items pi
		JOIN tracks t ON pi.video_id = t.video_id
		WHERE pi.playlist_id = ?
		ORDER BY pi.position ASC
		LIMIT ? OFFSET ?
	`, playlistID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query playlist items: %w", err)
	}
	defer rows.Close()

	var items []PlaylistItem
	for rows.Next() {
		var item PlaylistItem
		var album, thumb sql.NullString
		if err := rows.Scan(
			&item.ItemID, &item.PlaylistID, &item.Position, &item.VideoID, &item.AddedAt,
			&item.Track.Title, &item.Track.Artist, &album, &item.Track.Duration, &thumb, &item.Track.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan playlist item: %w", err)
		}
		item.Track.ID = item.VideoID
		if album.Valid {
			item.Track.Album = album.String
		}
		if thumb.Valid {
			item.Track.Thumbnail = thumb.String
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// UpdatePlaylist updates playlist name with optimistic concurrency check.
func (s *Store) UpdatePlaylist(ctx context.Context, userID int64, playlistID string, name string, expectedVersion *int64, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var curVer int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&curVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("get playlist version: %w", err)
	}

	if expectedVersion != nil && *expectedVersion != curVer {
		return curVer, ErrVersionConflict
	}

	if now == 0 {
		now = time.Now().UTC().Unix()
	}
	newVer := curVer + 1
	_, err = tx.ExecContext(ctx, `
		UPDATE playlists SET name = ?, version = ?, updated_at = ?
		WHERE id = ? AND user_id = ?
	`, name, newVer, now, playlistID, userID)
	if err != nil {
		return 0, fmt.Errorf("update playlist: %w", err)
	}

	if _, err := bumpPlaylistsVersionTx(ctx, tx, userID); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVer, nil
}

// DeletePlaylist deletes a playlist if owned by userID.
func (s *Store) DeletePlaylist(ctx context.Context, userID int64, playlistID string, expectedVersion *int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var curVer int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&curVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("get playlist version: %w", err)
	}

	if expectedVersion != nil && *expectedVersion != curVer {
		return ErrVersionConflict
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID)
	if err != nil {
		return fmt.Errorf("delete playlist: %w", err)
	}

	if _, err := bumpPlaylistsVersionTx(ctx, tx, userID); err != nil {
		return err
	}

	return tx.Commit()
}

// AddPlaylistItems adds tracks to a playlist at the specified position (or end if nil/past end).
// Re-indexes positions densely 0..n-1, bumps version and playlists_version.
func (s *Store) AddPlaylistItems(ctx context.Context, userID int64, playlistID string, tracks []Track, pos *int, expectedVersion *int64, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(tracks) == 0 {
		return 0, errors.New("no tracks provided")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var curVer int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&curVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("get playlist version: %w", err)
	}

	if expectedVersion != nil && *expectedVersion != curVer {
		return curVer, ErrVersionConflict
	}

	var currentCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlist_items WHERE playlist_id = ?`, playlistID).Scan(&currentCount); err != nil {
		return 0, fmt.Errorf("count playlist items: %w", err)
	}

	if currentCount+len(tracks) > 5000 {
		return curVer, ErrLimitExceeded
	}

	if now == 0 {
		now = time.Now().UTC().Unix()
	}

	// Upsert all tracks
	for _, t := range tracks {
		if err := upsertTrackTx(ctx, tx, t, now); err != nil {
			return 0, err
		}
	}

	// Fetch existing items ordered by position ASC
	rows, err := tx.QueryContext(ctx, `
		SELECT item_id FROM playlist_items WHERE playlist_id = ? ORDER BY position ASC
	`, playlistID)
	if err != nil {
		return 0, fmt.Errorf("query existing playlist items: %w", err)
	}
	var existingItemIDs []string
	for rows.Next() {
		var iid string
		if err := rows.Scan(&iid); err != nil {
			rows.Close()
			return 0, err
		}
		existingItemIDs = append(existingItemIDs, iid)
	}
	rows.Close()

	// Determine insertion point
	insertIdx := len(existingItemIDs)
	if pos != nil {
		if *pos < 0 {
			insertIdx = 0
		} else if *pos < len(existingItemIDs) {
			insertIdx = *pos
		}
	}

	// Generate new items
	type newItem struct {
		id      string
		videoID string
		addedAt int64
	}
	newItems := make([]newItem, len(tracks))
	for i, t := range tracks {
		newItems[i] = newItem{
			id:      randomURLSafeID(16),
			videoID: t.ID,
			addedAt: now,
		}
	}

	// Insert new items into database with temporary positions
	for _, it := range newItems {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO playlist_items (item_id, playlist_id, position, video_id, added_at)
			VALUES (?, ?, -1, ?, ?)
		`, it.id, playlistID, it.videoID, it.addedAt)
		if err != nil {
			return 0, fmt.Errorf("insert playlist item: %w", err)
		}
	}

	// Combine in final order
	var finalItemIDs []string
	finalItemIDs = append(finalItemIDs, existingItemIDs[:insertIdx]...)
	for _, it := range newItems {
		finalItemIDs = append(finalItemIDs, it.id)
	}
	finalItemIDs = append(finalItemIDs, existingItemIDs[insertIdx:]...)

	// Re-index dense 0..n-1
	for idx, iid := range finalItemIDs {
		_, err := tx.ExecContext(ctx, `
			UPDATE playlist_items SET position = ? WHERE item_id = ?
		`, idx, iid)
		if err != nil {
			return 0, fmt.Errorf("reindex item position: %w", err)
		}
	}

	newVer := curVer + 1
	_, err = tx.ExecContext(ctx, `
		UPDATE playlists SET version = ?, updated_at = ? WHERE id = ?
	`, newVer, now, playlistID)
	if err != nil {
		return 0, fmt.Errorf("bump playlist version: %w", err)
	}

	if _, err := bumpPlaylistsVersionTx(ctx, tx, userID); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVer, nil
}

// DeletePlaylistItem removes an item from a playlist and densely re-indexes remaining items.
func (s *Store) DeletePlaylistItem(ctx context.Context, userID int64, playlistID, itemID string, expectedVersion *int64, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var curVer int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&curVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("get playlist version: %w", err)
	}

	if expectedVersion != nil && *expectedVersion != curVer {
		return curVer, ErrVersionConflict
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ? AND item_id = ?`, playlistID, itemID)
	if err != nil {
		return 0, fmt.Errorf("delete playlist item: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, ErrNotFound
	}

	// Re-index remaining items dense 0..n-1
	rows, err := tx.QueryContext(ctx, `
		SELECT item_id FROM playlist_items WHERE playlist_id = ? ORDER BY position ASC
	`, playlistID)
	if err != nil {
		return 0, fmt.Errorf("query remaining items: %w", err)
	}
	var remaining []string
	for rows.Next() {
		var iid string
		if err := rows.Scan(&iid); err != nil {
			rows.Close()
			return 0, err
		}
		remaining = append(remaining, iid)
	}
	rows.Close()

	for idx, iid := range remaining {
		_, err := tx.ExecContext(ctx, `UPDATE playlist_items SET position = ? WHERE item_id = ?`, idx, iid)
		if err != nil {
			return 0, fmt.Errorf("reindex item: %w", err)
		}
	}

	if now == 0 {
		now = time.Now().UTC().Unix()
	}
	newVer := curVer + 1
	_, err = tx.ExecContext(ctx, `UPDATE playlists SET version = ?, updated_at = ? WHERE id = ?`, newVer, now, playlistID)
	if err != nil {
		return 0, fmt.Errorf("update playlist version: %w", err)
	}

	if _, err := bumpPlaylistsVersionTx(ctx, tx, userID); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVer, nil
}

// MovePlaylistItem reorders an item to toPos and densely re-indexes.
func (s *Store) MovePlaylistItem(ctx context.Context, userID int64, playlistID, itemID string, toPos int, expectedVersion *int64, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var curVer int64
	if err := tx.QueryRowContext(ctx, `SELECT version FROM playlists WHERE id = ? AND user_id = ?`, playlistID, userID).Scan(&curVer); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("get playlist version: %w", err)
	}

	if expectedVersion != nil && *expectedVersion != curVer {
		return curVer, ErrVersionConflict
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT item_id FROM playlist_items WHERE playlist_id = ? ORDER BY position ASC
	`, playlistID)
	if err != nil {
		return 0, fmt.Errorf("query items: %w", err)
	}
	var items []string
	foundIdx := -1
	for rows.Next() {
		var iid string
		if err := rows.Scan(&iid); err != nil {
			rows.Close()
			return 0, err
		}
		if iid == itemID {
			foundIdx = len(items)
		}
		items = append(items, iid)
	}
	rows.Close()

	if foundIdx == -1 {
		return 0, ErrNotFound
	}

	// Remove item from slice and insert at toPos
	items = append(items[:foundIdx], items[foundIdx+1:]...)
	if toPos < 0 {
		toPos = 0
	} else if toPos > len(items) {
		toPos = len(items)
	}
	items = append(items[:toPos], append([]string{itemID}, items[toPos:]...)...)

	// Re-index all
	for idx, iid := range items {
		_, err := tx.ExecContext(ctx, `UPDATE playlist_items SET position = ? WHERE item_id = ?`, idx, iid)
		if err != nil {
			return 0, fmt.Errorf("reindex item: %w", err)
		}
	}

	if now == 0 {
		now = time.Now().UTC().Unix()
	}
	newVer := curVer + 1
	_, err = tx.ExecContext(ctx, `UPDATE playlists SET version = ?, updated_at = ? WHERE id = ?`, newVer, now, playlistID)
	if err != nil {
		return 0, fmt.Errorf("update playlist version: %w", err)
	}

	if _, err := bumpPlaylistsVersionTx(ctx, tx, userID); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVer, nil
}

// AddHistory records played tracks. Duplicates of (user_id, video_id, played_at) are ignored.
// Returns the count of accepted (inserted) entries. Bumps history_version if accepted > 0.
func (s *Store) AddHistory(ctx context.Context, userID int64, entries []HistoryEntry) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(entries) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Unix()
	accepted := 0
	for _, e := range entries {
		if err := upsertTrackTx(ctx, tx, e.Track, now); err != nil {
			return 0, err
		}

		var devID any
		if e.DeviceID != nil && *e.DeviceID != "" {
			devID = *e.DeviceID
		}

		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO history (user_id, video_id, played_at, played_ms, device_id, source)
			VALUES (?, ?, ?, ?, ?, ?)
		`, userID, e.Track.ID, e.PlayedAt, e.PlayedMs, devID, e.Source)
		if err != nil {
			return 0, fmt.Errorf("insert history entry: %w", err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			accepted++
		}
	}

	if accepted > 0 {
		if _, err := bumpHistoryVersionTx(ctx, tx, userID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return accepted, nil
}

// GetHistory returns playback history newest first with pagination.
func (s *Store) GetHistory(ctx context.Context, userID int64, limit, offset int) ([]HistoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.user_id, h.video_id, h.played_at, h.played_ms, h.device_id, h.source,
		       t.title, t.artist, t.album, t.duration_s, t.thumb_url, t.updated_at
		FROM history h
		JOIN tracks t ON h.video_id = t.video_id
		WHERE h.user_id = ?
		ORDER BY h.played_at DESC, h.id DESC
		LIMIT ? OFFSET ?
	`, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var out []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var devID, album, thumb sql.NullString
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.VideoID, &e.PlayedAt, &e.PlayedMs, &devID, &e.Source,
			&e.Track.Title, &e.Track.Artist, &album, &e.Track.Duration, &thumb, &e.Track.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan history entry: %w", err)
		}
		e.Track.ID = e.VideoID
		if devID.Valid {
			str := devID.String
			e.DeviceID = &str
		}
		if album.Valid {
			e.Track.Album = album.String
		}
		if thumb.Valid {
			e.Track.Thumbnail = thumb.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearHistory deletes all history rows for a user and bumps history_version.
func (s *Store) ClearHistory(ctx context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM history WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("clear history: %w", err)
	}

	if _, err := bumpHistoryVersionTx(ctx, tx, userID); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteHistoryItem deletes one history entry for a user and bumps history_version.
func (s *Store) DeleteHistoryItem(ctx context.Context, userID int64, historyID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM history WHERE user_id = ? AND id = ?`, userID, historyID)
	if err != nil {
		return fmt.Errorf("delete history item: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if _, err := bumpHistoryVersionTx(ctx, tx, userID); err != nil {
		return err
	}

	return tx.Commit()
}

// PruneHistory prunes history keeping the newest keepPerUser rows per user.
func (s *Store) PruneHistory(ctx context.Context, keepPerUser int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		DELETE FROM history WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY played_at DESC, id DESC) as rn
				FROM history
			) WHERE rn > ?
		)
	`, keepPerUser)
	if err != nil {
		return 0, fmt.Errorf("prune history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune history rows affected: %w", err)
	}
	return n, nil
}
