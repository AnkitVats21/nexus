package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	// Light settings for the small pilot box (412 MB RAM): 64 MiB per login
	// pushed it into swap and logins took 20 s. Older hashes keep their own
	// settings and are re-saved with these at the next sign-in.
	argonMemory    = 8 * 1024 // 8 MiB = 8192 KiB
	argonTime      = 1
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
	minPasswordLen = 10
	argonVersion   = 19
)

var (
	ErrPasswordTooShort = errors.New("password must be at least 10 characters")
	ErrInvalidUsername  = errors.New("username must be 2-32 characters of letters, digits, '.', '_' or '-'")
	ErrInvalidHash      = errors.New("invalid password hash format")
	ErrBusy             = errors.New("service busy, please try again")
	usernameRegex       = regexp.MustCompile(`^[a-zA-Z0-9._-]{2,32}$`)

	argonSem     = make(chan struct{}, 2)
	argonTimeout = 3 * time.Second
)

func acquireArgon() (func(), error) {
	timer := time.NewTimer(argonTimeout)
	defer timer.Stop()

	select {
	case argonSem <- struct{}{}:
		return func() { <-argonSem }, nil
	case <-timer.C:
		return nil, ErrBusy
	}
}

// SetArgonTimeoutForTest temporarily overrides the argon semaphore timeout.
func SetArgonTimeoutForTest(d time.Duration) func() {
	orig := argonTimeout
	argonTimeout = d
	return func() { argonTimeout = orig }
}

// HoldArgonSlotsForTest occupies n argon semaphore slots until the returned func is called.
func HoldArgonSlotsForTest(n int) func() {
	for i := 0; i < n; i++ {
		argonSem <- struct{}{}
	}
	return func() {
		for i := 0; i < n; i++ {
			<-argonSem
		}
	}
}

// ValidateUsername checks if username matches [a-z0-9._-]{2,32}.
func ValidateUsername(username string) error {
	if !usernameRegex.MatchString(username) {
		return ErrInvalidUsername
	}
	return nil
}

// ValidatePassword checks if password meets minimum length requirement (10 characters).
func ValidatePassword(password string) error {
	if len(password) < minPasswordLen {
		return ErrPasswordTooShort
	}
	return nil
}

// HashPassword hashes a password using Argon2id with 8 MiB, t=1, p=1.
func HashPassword(password string) (string, error) {
	if len(password) < minPasswordLen {
		return "", ErrPasswordTooShort
	}

	release, err := acquireArgon()
	if err != nil {
		return "", err
	}
	defer release()

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Key := base64.RawStdEncoding.EncodeToString(key)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemory, argonTime, argonThreads, b64Salt, b64Key), nil
}

// NeedsRehash reports whether an encoded hash was made with other settings
// than the current ones.
func NeedsRehash(encodedHash string) bool {
	return !strings.HasPrefix(encodedHash, fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$",
		argonVersion, argonMemory, argonTime, argonThreads))
}

// VerifyPassword checks if a password matches the standard encoded Argon2id hash.
func VerifyPassword(password, encodedHash string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}

	var memory uint32
	var time uint32
	var threads uint8

	for _, param := range strings.Split(parts[3], ",") {
		kv := strings.SplitN(param, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "m":
			m, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return false, ErrInvalidHash
			}
			memory = uint32(m)
		case "t":
			t, err := strconv.ParseUint(kv[1], 10, 32)
			if err != nil {
				return false, ErrInvalidHash
			}
			time = uint32(t)
		case "p":
			p, err := strconv.ParseUint(kv[1], 10, 8)
			if err != nil {
				return false, ErrInvalidHash
			}
			threads = uint8(p)
		}
	}

	if memory == 0 || time == 0 || threads == 0 {
		return false, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}

	release, err := acquireArgon()
	if err != nil {
		return false, err
	}
	defer release()

	derivedKey := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(key)))

	if subtle.ConstantTimeCompare(derivedKey, key) == 1 {
		return true, nil
	}
	return false, nil
}

// GenerateTempPassword creates a secure random temporary password for admin user creation/resets.
func GenerateTempPassword() string {
	const charset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	max := big.NewInt(int64(len(charset)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = charset[n.Int64()]
	}
	return string(b)
}
