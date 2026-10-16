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

func TestStoreDevices(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Create admin and member
	admin := &User{Username: "admin", DisplayName: "Admin", Role: "admin", PassHash: "h1"}
	if err := s.CreateUser(ctx, admin); err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	member := &User{Username: "member", DisplayName: "Member", Role: "member", PassHash: "h2"}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}

	// Verify GetFirstAdmin
	firstAdmin, err := s.GetFirstAdmin(ctx)
	if err != nil || firstAdmin.ID != admin.ID {
		t.Fatalf("GetFirstAdmin: %v, got %+v", err, firstAdmin)
	}

	// 2. Upsert device for admin
	tokHash1 := sha256.Sum256([]byte("token-device-1"))
	d1 := &Device{
		ID:        "nexus-001",
		OwnerID:   admin.ID,
		Name:      "Kitchen Speaker",
		Kind:      "esp32-s3",
		TokenHash: tokHash1,
	}
	if err := s.UpsertDevice(ctx, d1); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	// 3. GetDeviceByID
	got, err := s.GetDeviceByID(ctx, "nexus-001")
	if err != nil {
		t.Fatalf("GetDeviceByID: %v", err)
	}
	if got.Name != "Kitchen Speaker" || got.OwnerID != admin.ID || got.TokenHash != tokHash1 {
		t.Fatalf("unexpected device: %+v", got)
	}

	// 4. GetDeviceByTokenHash
	gotTok, err := s.GetDeviceByTokenHash(ctx, tokHash1)
	if err != nil || gotTok.ID != "nexus-001" {
		t.Fatalf("GetDeviceByTokenHash: %v, got %+v", err, gotTok)
	}

	// 5. UpdateDeviceName & UpdateDeviceLastSeen
	if err := s.UpdateDeviceName(ctx, "nexus-001", "Living Room"); err != nil {
		t.Fatalf("UpdateDeviceName: %v", err)
	}
	now := time.Now().UTC().Unix()
	if err := s.UpdateDeviceLastSeen(ctx, "nexus-001", now, "1.4.0"); err != nil {
		t.Fatalf("UpdateDeviceLastSeen: %v", err)
	}
	gotUpdated, err := s.GetDeviceByID(ctx, "nexus-001")
	if err != nil || gotUpdated.Name != "Living Room" || gotUpdated.LastFW != "1.4.0" || gotUpdated.LastSeen != now {
		t.Fatalf("unexpected updated device: %+v", gotUpdated)
	}

	// 6. Re-claim device by member (new owner, new token)
	tokHash2 := sha256.Sum256([]byte("token-device-reclaimed"))
	dReclaim := &Device{
		ID:        "nexus-001",
		OwnerID:   member.ID,
		Name:      "Member Speaker",
		Kind:      "esp32-s3",
		TokenHash: tokHash2,
	}
	if err := s.UpsertDevice(ctx, dReclaim); err != nil {
		t.Fatalf("UpsertDevice re-claim: %v", err)
	}
	gotReclaimed, err := s.GetDeviceByID(ctx, "nexus-001")
	if err != nil || gotReclaimed.OwnerID != member.ID || gotReclaimed.TokenHash != tokHash2 {
		t.Fatalf("re-claim failed: %+v", gotReclaimed)
	}

	// 7. ListDevicesForUser & ListAllDevices
	adminDevs, err := s.ListDevicesForUser(ctx, admin.ID)
	if err != nil || len(adminDevs) != 0 {
		t.Fatalf("expected 0 devices for admin, got %d", len(adminDevs))
	}
	memberDevs, err := s.ListDevicesForUser(ctx, member.ID)
	if err != nil || len(memberDevs) != 1 {
		t.Fatalf("expected 1 device for member, got %d", len(memberDevs))
	}
	allDevs, err := s.ListAllDevices(ctx)
	if err != nil || len(allDevs) != 1 {
		t.Fatalf("expected 1 device in all, got %d", len(allDevs))
	}

	// 8. DeleteDevice
	if err := s.DeleteDevice(ctx, "nexus-001"); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	if _, err := s.GetDeviceByID(ctx, "nexus-001"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}

	// 9. Cascade delete on user delete
	dCascade := &Device{
		ID:        "nexus-002",
		OwnerID:   member.ID,
		Name:      "Bedroom",
		Kind:      "orbit",
		TokenHash: sha256.Sum256([]byte("orbit-tok")),
	}
	if err := s.UpsertDevice(ctx, dCascade); err != nil {
		t.Fatalf("UpsertDevice cascade: %v", err)
	}
	if err := s.DeleteUser(ctx, member.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.GetDeviceByID(ctx, "nexus-002"); err != ErrNotFound {
		t.Fatalf("expected device to be cascade deleted with user, got %v", err)
	}
}

func TestStorePairingCodes(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	u := &User{Username: "bob", DisplayName: "Bob", Role: "member", PassHash: "h"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	now := time.Now().UTC().Unix()
	hash1 := sha256.Sum256([]byte("pepper+123456"))
	hash2 := sha256.Sum256([]byte("pepper+654321"))
	hashExp := sha256.Sum256([]byte("pepper+000000"))

	pc1 := &PairingCode{CodeHash: hash1, UserID: u.ID, ExpiresAt: now + 600}
	pc2 := &PairingCode{CodeHash: hash2, UserID: u.ID, ExpiresAt: now + 300}
	pcExp := &PairingCode{CodeHash: hashExp, UserID: u.ID, ExpiresAt: now - 10}

	if err := s.CreatePairingCode(ctx, pc1); err != nil {
		t.Fatalf("CreatePairingCode 1: %v", err)
	}
	if err := s.CreatePairingCode(ctx, pc2); err != nil {
		t.Fatalf("CreatePairingCode 2: %v", err)
	}
	if err := s.CreatePairingCode(ctx, pcExp); err != nil {
		t.Fatalf("CreatePairingCode exp: %v", err)
	}

	// CountActivePairingCodes should count only unexpired
	count, err := s.CountActivePairingCodes(ctx, u.ID, now)
	if err != nil || count != 2 {
		t.Fatalf("CountActivePairingCodes: err=%v, count=%d (expected 2)", err, count)
	}

	// ListActivePairingCodes
	active, err := s.ListActivePairingCodes(ctx, u.ID, now)
	if err != nil || len(active) != 2 {
		t.Fatalf("ListActivePairingCodes: err=%v, len=%d (expected 2)", err, len(active))
	}

	// GetPairingCode
	got, err := s.GetPairingCode(ctx, hash1)
	if err != nil || got.UserID != u.ID || got.ExpiresAt != now+600 {
		t.Fatalf("GetPairingCode: %v, got %+v", err, got)
	}

	// DeletePairingCode
	if err := s.DeletePairingCode(ctx, hash1); err != nil {
		t.Fatalf("DeletePairingCode: %v", err)
	}
	if _, err := s.GetPairingCode(ctx, hash1); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}

	// DeleteExpiredPairingCodes
	if err := s.DeleteExpiredPairingCodes(ctx, now); err != nil {
		t.Fatalf("DeleteExpiredPairingCodes: %v", err)
	}
	if _, err := s.GetPairingCode(ctx, hashExp); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for expired code, got %v", err)
	}

	// DeletePairingCodesForUser
	if err := s.DeletePairingCodesForUser(ctx, u.ID); err != nil {
		t.Fatalf("DeletePairingCodesForUser: %v", err)
	}
	countAfter, err := s.CountActivePairingCodes(ctx, u.ID, now)
	if err != nil || countAfter != 0 {
		t.Fatalf("expected 0 pairing codes after DeletePairingCodesForUser, got %d", countAfter)
	}
}

func TestClaimPairingCode(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newTestStore(t)
	defer cleanup()
	now := time.Now().UTC().Unix()

	u := &User{
		Username:    "carol",
		DisplayName: "Carol White",
		PassHash:    "hash",
		Role:        "member",
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	var codeHash [32]byte
	copy(codeHash[:], []byte("claim-code-hash-123456789012345"))
	if err := s.CreatePairingCode(ctx, &PairingCode{
		CodeHash:  codeHash,
		UserID:    u.ID,
		ExpiresAt: now + 600,
	}); err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}

	var tokHash [32]byte
	copy(tokHash[:], []byte("dev-tok-hash-123456789012345678"))
	dev := &Device{
		ID:        "dev-carol-1",
		Name:      "Living Room",
		Kind:      "esp32-s3",
		TokenHash: tokHash,
		LastSeen:  now,
		LastFW:    "1.0.0",
	}

	// 1. Successful atomic claim
	owner, err := s.ClaimPairingCode(ctx, codeHash, now, dev)
	if err != nil {
		t.Fatalf("ClaimPairingCode failed: %v", err)
	}
	if owner.ID != u.ID || owner.Username != "carol" {
		t.Fatalf("unexpected owner: %+v", owner)
	}

	// Device must now exist in store
	gotDev, err := s.GetDeviceByID(ctx, "dev-carol-1")
	if err != nil || gotDev.OwnerID != u.ID || gotDev.Name != "Living Room" {
		t.Fatalf("GetDeviceByID after claim: %v, got %+v", err, gotDev)
	}

	// Pairing code must be consumed
	if _, err := s.GetPairingCode(ctx, codeHash); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for consumed pairing code, got %v", err)
	}

	// 2. Second claim with the same code fails with ErrNotFound
	_, err = s.ClaimPairingCode(ctx, codeHash, now, dev)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound on second claim, got %v", err)
	}

	// 3. Expired code fails
	var expCodeHash [32]byte
	copy(expCodeHash[:], []byte("exp-code-hash-123456789012345678"))
	_ = s.CreatePairingCode(ctx, &PairingCode{
		CodeHash:  expCodeHash,
		UserID:    u.ID,
		ExpiresAt: now - 10,
	})
	_, err = s.ClaimPairingCode(ctx, expCodeHash, now, dev)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for expired code, got %v", err)
	}
}

func TestPushTokensLifecycleAndCascade(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// 1. Get non-existent token
	if _, err := s.GetPushToken(ctx, "non-existent-device"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for non-existent device, got %v", err)
	}

	// Setup user & device
	u := &User{
		Username:    "dave",
		DisplayName: "Dave",
		Role:        "member",
		PassHash:    "dummy_hash",
	}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	devID := "dev-phone-123"
	tokHash := sha256.Sum256([]byte("dummy-device-token"))
	dev := &Device{
		ID:        devID,
		OwnerID:   u.ID,
		Name:      "Dave's Phone",
		Kind:      "android",
		TokenHash: tokHash,
	}
	if err := s.UpsertDevice(ctx, dev); err != nil {
		t.Fatalf("UpsertDevice: %v", err)
	}

	// 2. UpsertPushToken
	pt := &PushToken{
		DeviceID: devID,
		Provider: "fcm",
		Token:    "fcm-reg-token-abc",
	}
	if err := s.UpsertPushToken(ctx, pt); err != nil {
		t.Fatalf("UpsertPushToken: %v", err)
	}

	// 3. GetPushToken
	got, err := s.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken: %v", err)
	}
	if got.DeviceID != devID || got.Provider != "fcm" || got.Token != "fcm-reg-token-abc" {
		t.Fatalf("unexpected push token: %+v", got)
	}
	if got.FailCount != 0 || got.LastOKAt != 0 || got.UpdatedAt == 0 {
		t.Fatalf("unexpected push token fields: %+v", got)
	}

	// 4. MarkPushOK
	if err := s.MarkPushOK(ctx, devID); err != nil {
		t.Fatalf("MarkPushOK: %v", err)
	}
	gotOK, err := s.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken after MarkPushOK: %v", err)
	}
	if gotOK.LastOKAt == 0 {
		t.Fatalf("expected LastOKAt to be updated, got 0")
	}

	// 5. MarkPushFail
	if err := s.MarkPushFail(ctx, devID); err != nil {
		t.Fatalf("MarkPushFail: %v", err)
	}
	if err := s.MarkPushFail(ctx, devID); err != nil {
		t.Fatalf("MarkPushFail second: %v", err)
	}
	gotFail, err := s.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken after MarkPushFail: %v", err)
	}
	if gotFail.FailCount != 2 {
		t.Fatalf("expected FailCount=2, got %d", gotFail.FailCount)
	}

	// 6. Upsert new token resets fail_count
	pt2 := &PushToken{
		DeviceID: devID,
		Provider: "fcm",
		Token:    "fcm-reg-token-xyz",
	}
	if err := s.UpsertPushToken(ctx, pt2); err != nil {
		t.Fatalf("UpsertPushToken pt2: %v", err)
	}
	got2, err := s.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken pt2: %v", err)
	}
	if got2.Token != "fcm-reg-token-xyz" || got2.FailCount != 0 {
		t.Fatalf("expected token updated and FailCount reset to 0, got %+v", got2)
	}

	// 7. DeletePushToken
	if err := s.DeletePushToken(ctx, devID); err != nil {
		t.Fatalf("DeletePushToken: %v", err)
	}
	if _, err := s.GetPushToken(ctx, devID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after DeletePushToken, got %v", err)
	}
	if err := s.DeletePushToken(ctx, devID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound on duplicate delete, got %v", err)
	}

	// 8. Cascade on device delete
	if err := s.UpsertPushToken(ctx, pt2); err != nil {
		t.Fatalf("UpsertPushToken before cascade test: %v", err)
	}
	// Verify token is present
	if _, err := s.GetPushToken(ctx, devID); err != nil {
		t.Fatalf("GetPushToken before device delete: %v", err)
	}
	// Delete device
	if err := s.DeleteDevice(ctx, devID); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	// Token must have been deleted via CASCADE
	if _, err := s.GetPushToken(ctx, devID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for push token after device delete (CASCADE), got %v", err)
	}
}

func TestPushTokensConditionalOperations(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.CreateUser(ctx, &User{Username: "cond", DisplayName: "Cond User", Role: "member", PassHash: "h"})
	u, _ := s.GetUserByUsername(ctx, "cond")
	devID := "dev-cond-1"
	_ = s.UpsertDevice(ctx, &Device{ID: devID, OwnerID: u.ID, Name: "D", Kind: "android"})

	// Setup initial token A
	_ = s.UpsertPushToken(ctx, &PushToken{DeviceID: devID, Provider: "fcm", Token: "tokenA"})

	// 1. MarkPushOKIf: mismatch does not update, match updates
	if err := s.MarkPushOKIf(ctx, devID, "tokenWrong"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for MarkPushOKIf on token mismatch, got %v", err)
	}
	got, _ := s.GetPushToken(ctx, devID)
	if got.LastOKAt != 0 {
		t.Fatalf("expected LastOKAt to be 0 after failed MarkPushOKIf, got %d", got.LastOKAt)
	}
	if err := s.MarkPushOKIf(ctx, devID, "tokenA"); err != nil {
		t.Fatalf("MarkPushOKIf with match failed: %v", err)
	}
	got, _ = s.GetPushToken(ctx, devID)
	if got.LastOKAt == 0 {
		t.Fatalf("expected LastOKAt > 0 after matching MarkPushOKIf")
	}

	// 2. MarkPushFailIf: mismatch does not increment, match increments
	if err := s.MarkPushFailIf(ctx, devID, "tokenWrong"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for MarkPushFailIf on token mismatch, got %v", err)
	}
	got, _ = s.GetPushToken(ctx, devID)
	if got.FailCount != 0 {
		t.Fatalf("expected FailCount to remain 0 after failed MarkPushFailIf")
	}
	if err := s.MarkPushFailIf(ctx, devID, "tokenA"); err != nil {
		t.Fatalf("MarkPushFailIf with match failed: %v", err)
	}
	got, _ = s.GetPushToken(ctx, devID)
	if got.FailCount != 1 {
		t.Fatalf("expected FailCount to be 1 after matching MarkPushFailIf, got %d", got.FailCount)
	}

	// 3. DeletePushTokenIf: race simulation
	// Device uploaded token B while send for token A was happening
	_ = s.UpsertPushToken(ctx, &PushToken{DeviceID: devID, Provider: "fcm", Token: "tokenB"})

	// DeletePushTokenIf for old tokenA must return ErrNotFound and tokenB MUST survive
	if err := s.DeletePushTokenIf(ctx, devID, "tokenA"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound deleting stale tokenA, got %v", err)
	}
	surviving, err := s.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken failed: %v", err)
	}
	if surviving.Token != "tokenB" {
		t.Fatalf("tokenB was unexpectedly overwritten/deleted: %+v", surviving)
	}

	// DeletePushTokenIf for matching tokenB succeeds
	if err := s.DeletePushTokenIf(ctx, devID, "tokenB"); err != nil {
		t.Fatalf("DeletePushTokenIf for matching tokenB failed: %v", err)
	}
	if _, err := s.GetPushToken(ctx, devID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after deleting tokenB, got %v", err)
	}
}
