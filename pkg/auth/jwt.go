package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"nexus/pkg/store"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidJWT = errors.New("invalid or expired jwt token")
)

// JWTClaims holds parsed and verified claims from an access token.
type JWTClaims struct {
	UserID    int64
	Username  string
	Role      string
	ExpiresAt time.Time
	JTI       string
}

// JWTService handles signing and verifying Ed25519 JWT access tokens.
type JWTService struct {
	privKey ed25519.PrivateKey
	pubKey  ed25519.PublicKey
	kid     string
}

// KeyID returns the 16-character hex representation of the first 8 bytes of sha256(pubKey).
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// NewJWTService creates a JWTService with the given Ed25519 private key.
func NewJWTService(priv ed25519.PrivateKey) *JWTService {
	pub := priv.Public().(ed25519.PublicKey)
	return &JWTService{
		privKey: priv,
		pubKey:  pub,
		kid:     KeyID(pub),
	}
}

// KeyID returns the key ID for this JWTService.
func (s *JWTService) KeyID() string {
	return s.kid
}

// LoadOrGenerateJWTKey loads an Ed25519 private key from path, or generates and saves one with 0600 permissions.
func LoadOrGenerateJWTKey(keyPath string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(keyPath)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("no PEM data found in key file %s", keyPath)
		}
		parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
		}
		edKey, ok := parsedKey.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key in %s is not Ed25519", keyPath)
		}
		return edKey, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read jwt key file: %w", err)
	}

	// Generate new key
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS8 key: %w", err)
	}

	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})

	dir := filepath.Dir(keyPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create jwt key dir: %w", err)
		}
	}

	if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key file: %w", err)
	}

	return priv, nil
}

// SignUserToken generates a signed EdDSA JWT valid for 15 minutes.
func (j *JWTService) SignUserToken(u *store.User) (string, time.Time, error) {
	now := time.Now().UTC()
	exp := now.Add(15 * time.Minute)

	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", time.Time{}, fmt.Errorf("generate jti: %w", err)
	}
	jti := hex.EncodeToString(jtiBytes)

	claims := jwt.MapClaims{
		"iss":  "nexus-hub",
		"aud":  "nexus",
		"sub":  strconv.FormatInt(u.ID, 10),
		"name": u.Username,
		"role": u.Role,
		"iat":  now.Unix(),
		"exp":  exp.Unix(),
		"jti":  jti,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = j.kid

	signedStr, err := token.SignedString(j.privKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign jwt: %w", err)
	}

	return signedStr, exp, nil
}

// VerifyToken validates an access token strictly enforcing EdDSA, iss, aud, and exp with 30s leeway.
func (j *JWTService) VerifyToken(tokenStr string) (*JWTClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithIssuer("nexus-hub"),
		jwt.WithAudience("nexus"),
		jwt.WithLeeway(30*time.Second),
	)

	token, err := parser.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != "EdDSA" {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.pubKey, nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidJWT
	}

	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalidJWT
	}

	subStr, _ := mapClaims["sub"].(string)
	userID, err := strconv.ParseInt(subStr, 10, 64)
	if err != nil {
		return nil, ErrInvalidJWT
	}

	username, _ := mapClaims["name"].(string)
	role, _ := mapClaims["role"].(string)
	jti, _ := mapClaims["jti"].(string)

	var expTime time.Time
	if expFloat, ok := mapClaims["exp"].(float64); ok {
		expTime = time.Unix(int64(expFloat), 0)
	}

	return &JWTClaims{
		UserID:    userID,
		Username:  username,
		Role:      role,
		ExpiresAt: expTime,
		JTI:       jti,
	}, nil
}

// JWKS returns the public key as a standard JWKS response map.
func (j *JWTService) JWKS() map[string]any {
	return map[string]any{
		"keys": []map[string]any{
			{
				"kty": "OKP",
				"crv": "Ed25519",
				"alg": "EdDSA",
				"use": "sig",
				"kid": j.kid,
				"x":   base64.RawURLEncoding.EncodeToString(j.pubKey),
			},
		},
	}
}

// Kid returns the key ID.
func (j *JWTService) Kid() string {
	return j.kid
}

// PublicKey returns the Ed25519 public key.
func (j *JWTService) PublicKey() ed25519.PublicKey {
	return j.pubKey
}

// GenerateRefreshToken creates a cryptographically secure 32-byte base64url refresh token.
func GenerateRefreshToken() (string, [32]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", [32]byte{}, fmt.Errorf("generate random bytes: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	return token, hash, nil
}

// HashRefreshToken returns the sha256 of a refresh token.
func HashRefreshToken(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// GenerateFamilyID creates a unique identifier for a refresh token family.
func GenerateFamilyID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
