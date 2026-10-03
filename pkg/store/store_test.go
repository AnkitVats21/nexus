package store

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nexus-store-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "test.db")
	s, err := Open(dbPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("Open: %v", err)
	}
	cleanup := func() {
		s.Close()
		os.RemoveAll(dir)
	}
	return s, cleanup
}

func TestStoreMigrationsAndUsers(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Initial user count should be 0
	count, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 users, got %d", count)
	}

	// Create admin user
	u1 := &User{
		Username:    "admin",
		DisplayName: "Administrator",
		Role:        "admin",
		PassHash:    "dummy_hash",
		MustChange:  false,
	}
	if err := s.CreateUser(ctx, u1); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u1.ID <= 0 {
		t.Fatalf("expected positive user ID, got %d", u1.ID)
	}

	// Conflict on duplicate username (case-insensitive)
	uDuplicate := &User{
		Username:    "ADMIN",
		DisplayName: "Admin 2",
		Role:        "member",
		PassHash:    "dummy_hash_2",
	}
	if err := s.CreateUser(ctx, uDuplicate); err != ErrConflict {
		t.Fatalf("expected ErrConflict on duplicate username, got %v", err)
	}

	// Check count
	count, err = s.CountUsers(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expected 1 user, got count=%d, err=%v", count, err)
	}

	adminCount, err := s.CountAdmins(ctx)
	if err != nil || adminCount != 1 {
		t.Fatalf("expected 1 admin, got count=%d, err=%v", adminCount, err)
	}

	// Fetch by username (case-insensitive)
	fetched, err := s.GetUserByUsername(ctx, "AdMiN")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if fetched.ID != u1.ID || fetched.DisplayName != "Administrator" || fetched.Role != "admin" {
		t.Fatalf("unexpected fetched user: %+v", fetched)
	}

	// Fetch by ID
	fetchedID, err := s.GetUserByID(ctx, u1.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if fetchedID.Username != "admin" {
		t.Fatalf("unexpected fetched username: %s", fetchedID.Username)
	}

	// Create member user
	u2 := &User{
		Username:    "riya",
		DisplayName: "Riya",
		Role:        "member",
		PassHash:    "hash_riya",
		MustChange:  true,
	}
	if err := s.CreateUser(ctx, u2); err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	// Update password
	if err := s.UpdateUserPassword(ctx, u2.ID, "new_hash_riya", false); err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}
	u2Updated, _ := s.GetUserByID(ctx, u2.ID)
	if u2Updated.PassHash != "new_hash_riya" || u2Updated.MustChange {
		t.Fatalf("unexpected updated user: %+v", u2Updated)
	}

	// Update display name
	if err := s.UpdateUserDisplayName(ctx, u2.ID, "Riya S"); err != nil {
		t.Fatalf("UpdateUserDisplayName: %v", err)
	}
	u2Updated, _ = s.GetUserByID(ctx, u2.ID)
	if u2Updated.DisplayName != "Riya S" {
		t.Fatalf("unexpected display name: %s", u2Updated.DisplayName)
	}
}

func TestStoreSessionsAndCascade(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	u := &User{
		Username:    "bob",
		DisplayName: "Bob",
		Role:        "member",
		PassHash:    "some_hash",
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token1 := sha256.Sum256([]byte("session_token_1"))
	token2 := sha256.Sum256([]byte("session_token_2"))
	now := time.Now().UTC().Unix()

	sess1 := &Session{
		IDHash:    token1,
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now + 3600,
		UserAgent: "Mozilla/5.0",
		IP:        "127.0.0.1",
	}
	if err := s.CreateSession(ctx, sess1); err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}

	sess2 := &Session{
		IDHash:    token2,
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now + 7200,
		UserAgent: "Mobile Safari",
		IP:        "192.168.1.50",
	}
	if err := s.CreateSession(ctx, sess2); err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}

	// Retrieve session 1
	gotSess, gotUser, err := s.GetSession(ctx, token1)
	if err != nil {
		t.Fatalf("GetSession 1: %v", err)
	}
	if gotSess.UserID != u.ID || gotUser.Username != "bob" {
		t.Fatalf("unexpected session/user: sess=%+v, user=%+v", gotSess, gotUser)
	}

	// Touch session 1
	if err := s.TouchSession(ctx, token1, now+10000); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	gotSess, _, _ = s.GetSession(ctx, token1)
	if gotSess.ExpiresAt != now+10000 {
		t.Fatalf("expected expires_at %d, got %d", now+10000, gotSess.ExpiresAt)
	}

	// Delete other sessions for user except token1
	if err := s.DeleteSessionsForUserExcept(ctx, u.ID, &token1); err != nil {
		t.Fatalf("DeleteSessionsForUserExcept: %v", err)
	}
	if _, _, err := s.GetSession(ctx, token2); err != ErrNotFound {
		t.Fatalf("expected session 2 to be deleted, got err=%v", err)
	}
	if _, _, err := s.GetSession(ctx, token1); err != nil {
		t.Fatalf("expected session 1 to remain, got err=%v", err)
	}

	// Delete user and verify CASCADE deleted session 1
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, _, err := s.GetSession(ctx, token1); err != ErrNotFound {
		t.Fatalf("expected session 1 to be deleted on user cascade, got err=%v", err)
	}
}

func TestStoreExpiredSessions(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	u := &User{
		Username:    "alice",
		DisplayName: "Alice",
		Role:        "member",
		PassHash:    "some_hash",
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token := sha256.Sum256([]byte("expired_token"))
	now := time.Now().UTC().Unix()
	sess := &Session{
		IDHash:    token,
		UserID:    u.ID,
		CreatedAt: now - 3600,
		ExpiresAt: now - 10, // already expired
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// GetSession should filter out expired sessions
	if _, _, err := s.GetSession(ctx, token); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for expired session, got %v", err)
	}

	// Run DeleteExpiredSessions
	if err := s.DeleteExpiredSessions(ctx); err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
}
