package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	MaxActivePairingCodes = 3
	PairingCodeTTL        = 10 * time.Minute
	DeviceTokenBytes      = 32
)

var (
	ErrMaxPairingCodesReached = errors.New("maximum 3 active pairing codes reached")
	ErrInvalidPairingCode     = errors.New("invalid pairing code")
)

// GeneratePairingCode creates a random 6-digit pairing code formatted as "123-456".
func GeneratePairingCode() string {
	n1, err := rand.Int(rand.Reader, big.NewInt(1000))
	if err != nil {
		panic(err)
	}
	n2, err := rand.Int(rand.Reader, big.NewInt(1000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%03d-%03d", n1.Int64(), n2.Int64())
}

// NormalizePairingCode strips hyphens and whitespace from a pairing code.
func NormalizePairingCode(code string) string {
	s := strings.TrimSpace(code)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

// HashPairingCode computes sha256(pepper || canonicalCode).
func HashPairingCode(pepper []byte, code string) [32]byte {
	canonical := NormalizePairingCode(code)
	h := sha256.New()
	h.Write(pepper)
	h.Write([]byte(canonical))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// GenerateDeviceToken creates a cryptographically secure 32-byte token encoded as base64url.
func GenerateDeviceToken() string {
	b := make([]byte, DeviceTokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashDeviceToken returns the sha256 hash of a device token.
func HashDeviceToken(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// LoadOrCreatePepper loads the pepper from pepperPath, or generates 32 random bytes and creates the file with mode 0600.
func LoadOrCreatePepper(pepperPath string) ([]byte, error) {
	if pepperPath == "" || pepperPath == ":memory:" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("generate in-memory pepper: %w", err)
		}
		return b, nil
	}

	data, err := os.ReadFile(pepperPath)
	if err == nil {
		if len(data) < 16 {
			return nil, fmt.Errorf("pepper file %s is too short (%d bytes, min 16)", pepperPath, len(data))
		}
		return data, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read pepper file: %w", err)
	}

	dir := filepath.Dir(pepperPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create pepper dir: %w", err)
		}
	}

	pepper := make([]byte, 32)
	if _, err := rand.Read(pepper); err != nil {
		return nil, fmt.Errorf("generate pepper: %w", err)
	}

	f, err := os.OpenFile(pepperPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return os.ReadFile(pepperPath)
		}
		return nil, fmt.Errorf("create pepper file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(pepper); err != nil {
		return nil, fmt.Errorf("write pepper: %w", err)
	}

	return pepper, nil
}
