package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"nexus/pkg/store"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWT_KeyManagementAndPermissions(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "jwt_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tempDir)

	keyPath := filepath.Join(tempDir, "jwt_ed25519.pem")

	// 1. Generate key when missing
	privKey, err := LoadOrGenerateJWTKey(keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenerateJWTKey generate failed: %v", err)
	}
	if privKey == nil {
		t.Fatal("expected non-nil private key")
	}

	// Verify permissions mode 0600
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file failed: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("expected key file mode 0600, got %o", fi.Mode().Perm())
	}

	// 2. Load existing key
	privKey2, err := LoadOrGenerateJWTKey(keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenerateJWTKey load failed: %v", err)
	}
	if !privKey.Equal(privKey2) {
		t.Fatal("expected loaded key to equal generated key")
	}

	// 3. KeyID format (first 8 bytes of sha256 in hex = 16 hex chars)
	pubKey := privKey.Public().(ed25519.PublicKey)
	kid := KeyID(pubKey)
	expectedHash := sha256.Sum256(pubKey)
	expectedKid := hex.EncodeToString(expectedHash[:8])
	if kid != expectedKid {
		t.Fatalf("expected kid %q, got %q", expectedKid, kid)
	}
	if len(kid) != 16 {
		t.Fatalf("expected kid length 16, got %d (%s)", len(kid), kid)
	}
}

func TestJWT_SignAndVerify(t *testing.T) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}

	svc := NewJWTService(privKey)
	u := &store.User{
		ID:       42,
		Username: "alice",
		Role:     "member",
		Status:   "active",
	}

	tokenStr, exp, err := svc.SignUserToken(u)
	if err != nil {
		t.Fatalf("SignUserToken failed: %v", err)
	}
	if tokenStr == "" {
		t.Fatal("expected non-empty token string")
	}
	if exp.Before(time.Now().Add(14 * time.Minute)) {
		t.Fatalf("expected expiry ~15m in future, got %v", exp)
	}

	// Verify token
	claims, err := svc.VerifyToken(tokenStr)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}
	if claims.UserID != 42 || claims.Username != "alice" || claims.Role != "member" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.JTI == "" {
		t.Fatal("expected non-empty JTI claim")
	}

	// JWKS shape
	jwks := svc.JWKS()
	keysList, ok := jwks["keys"].([]map[string]any)
	if !ok || len(keysList) != 1 {
		t.Fatalf("expected 1 key in JWKS, got %+v", jwks)
	}
	k := keysList[0]
	if k["kty"] != "OKP" || k["crv"] != "Ed25519" || k["alg"] != "EdDSA" || k["use"] != "sig" {
		t.Fatalf("invalid JWKS parameters: %+v", k)
	}
	if k["kid"] != svc.Kid() {
		t.Fatalf("expected JWKS kid %s, got %s", svc.Kid(), k["kid"])
	}
	if k["x"] != base64.RawURLEncoding.EncodeToString(svc.PublicKey()) {
		t.Fatalf("unexpected JWKS public key x: %v", k["x"])
	}
}

func TestJWT_StrictVerificationRejections(t *testing.T) {
	_, privKey, _ := ed25519.GenerateKey(rand.Reader)
	svc := NewJWTService(privKey)

	// 1. Reject alg "none"
	claims := jwt.MapClaims{
		"iss":  "nexus-hub",
		"aud":  "nexus",
		"sub":  "1",
		"name": "hacker",
		"role": "admin",
		"exp":  time.Now().Add(15 * time.Minute).Unix(),
	}
	noneToken := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	noneStr, _ := noneToken.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := svc.VerifyToken(noneStr); err == nil {
		t.Fatal("expected VerifyToken to reject alg 'none'")
	}

	// 2. Reject HS256
	hsToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	hsStr, _ := hsToken.SignedString([]byte("secret"))
	if _, err := svc.VerifyToken(hsStr); err == nil {
		t.Fatal("expected VerifyToken to reject alg HS256")
	}

	// 3. Reject wrong audience
	wrongAudClaims := jwt.MapClaims{
		"iss":  "nexus-hub",
		"aud":  "wrong-aud",
		"sub":  "1",
		"name": "alice",
		"role": "member",
		"exp":  time.Now().Add(15 * time.Minute).Unix(),
	}
	wrongAudToken := jwt.NewWithClaims(jwt.SigningMethodEdDSA, wrongAudClaims)
	wrongAudToken.Header["kid"] = svc.Kid()
	wrongAudStr, _ := wrongAudToken.SignedString(privKey)
	if _, err := svc.VerifyToken(wrongAudStr); err == nil {
		t.Fatal("expected VerifyToken to reject wrong audience")
	}

	// 4. Reject wrong issuer
	wrongIssClaims := jwt.MapClaims{
		"iss":  "wrong-hub",
		"aud":  "nexus",
		"sub":  "1",
		"name": "alice",
		"role": "member",
		"exp":  time.Now().Add(15 * time.Minute).Unix(),
	}
	wrongIssToken := jwt.NewWithClaims(jwt.SigningMethodEdDSA, wrongIssClaims)
	wrongIssToken.Header["kid"] = svc.Kid()
	wrongIssStr, _ := wrongIssToken.SignedString(privKey)
	if _, err := svc.VerifyToken(wrongIssStr); err == nil {
		t.Fatal("expected VerifyToken to reject wrong issuer")
	}

	// 5. Reject expired token (beyond 30s leeway)
	expiredClaims := jwt.MapClaims{
		"iss":  "nexus-hub",
		"aud":  "nexus",
		"sub":  "1",
		"name": "alice",
		"role": "member",
		"exp":  time.Now().Add(-60 * time.Second).Unix(),
	}
	expiredToken := jwt.NewWithClaims(jwt.SigningMethodEdDSA, expiredClaims)
	expiredToken.Header["kid"] = svc.Kid()
	expiredStr, _ := expiredToken.SignedString(privKey)
	if _, err := svc.VerifyToken(expiredStr); err == nil {
		t.Fatal("expected VerifyToken to reject expired token")
	}

	// 6. Within 30s leeway is accepted
	leewayClaims := jwt.MapClaims{
		"iss":  "nexus-hub",
		"aud":  "nexus",
		"sub":  "1",
		"name": "alice",
		"role": "member",
		"exp":  time.Now().Add(-10 * time.Second).Unix(),
	}
	leewayToken := jwt.NewWithClaims(jwt.SigningMethodEdDSA, leewayClaims)
	leewayToken.Header["kid"] = svc.Kid()
	leewayStr, _ := leewayToken.SignedString(privKey)
	if _, err := svc.VerifyToken(leewayStr); err != nil {
		t.Fatalf("expected token within 30s leeway to be accepted, got error: %v", err)
	}
}

func TestRefreshHelpersAndRateLimiter(t *testing.T) {
	// 1. Refresh token generation
	tok, hash, err := GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken failed: %v", err)
	}
	if len(tok) == 0 {
		t.Fatal("expected non-empty refresh token")
	}
	if HashRefreshToken(tok) != hash {
		t.Fatal("HashRefreshToken mismatch")
	}

	famID := GenerateFamilyID()
	if len(famID) != 32 { // 16 bytes hex = 32 chars
		t.Fatalf("expected 32 hex chars for familyID, got %d (%s)", len(famID), famID)
	}

	// 2. Register rate limit (5 per IP per hour)
	rl := NewRateLimiter()
	ip := "192.168.1.100"
	for i := 0; i < 5; i++ {
		if !rl.RegisterAllowed(ip) {
			t.Fatalf("attempt %d should be allowed", i)
		}
		rl.RecordRegister(ip)
	}
	if rl.RegisterAllowed(ip) {
		t.Fatal("6th register attempt should be blocked by rate limit")
	}
}

func TestLoginGating(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open failed: %v", err)
	}
	defer st.Close()

	svc, err := NewService(st, "", "")
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	ctx := context.Background()
	passHash, _ := HashPassword("ValidPass123!")

	// 1. Active user login
	_ = st.CreateUser(ctx, &store.User{Username: "u_act", DisplayName: "Act", Role: "member", Status: "active", PassHash: passHash})
	u, err := svc.VerifyUserCredentials(ctx, "u_act", "ValidPass123!", "127.0.0.1")
	if err != nil || u == nil {
		t.Fatalf("expected active user login to succeed, got %v", err)
	}

	// 2. Pending user login gating
	_ = st.CreateUser(ctx, &store.User{Username: "u_pend", DisplayName: "Pend", Role: "member", Status: "pending", PassHash: passHash})
	// Wrong password returns generic ErrInvalidCredentials
	_, err = svc.VerifyUserCredentials(ctx, "u_pend", "WrongPass!", "127.0.0.1")
	if err != ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials on wrong password, got %v", err)
	}
	// Correct password returns ErrPendingApproval
	_, err = svc.VerifyUserCredentials(ctx, "u_pend", "ValidPass123!", "127.0.0.1")
	if err != ErrPendingApproval {
		t.Fatalf("expected ErrPendingApproval on correct password, got %v", err)
	}

	// 3. Disabled user login gating
	_ = st.CreateUser(ctx, &store.User{Username: "u_dis", DisplayName: "Dis", Role: "member", Status: "disabled", PassHash: passHash})
	// Wrong password returns generic ErrInvalidCredentials
	_, err = svc.VerifyUserCredentials(ctx, "u_dis", "WrongPass!", "127.0.0.1")
	if err != ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials on wrong password, got %v", err)
	}
	// Correct password returns ErrUserDisabled
	_, err = svc.VerifyUserCredentials(ctx, "u_dis", "ValidPass123!", "127.0.0.1")
	if err != ErrUserDisabled {
		t.Fatalf("expected ErrUserDisabled on correct password, got %v", err)
	}
}
