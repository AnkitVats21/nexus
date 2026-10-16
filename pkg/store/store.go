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
