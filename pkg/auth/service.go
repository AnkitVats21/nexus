package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"net"
	"nexus/pkg/store"
	"strings"
	"sync"
	"time"
)

var (
	ErrSetupNotNeeded        = errors.New("setup already completed")
	ErrInvalidSetupCode      = errors.New("invalid setup code")
	ErrInvalidCredentials    = errors.New("invalid username or password")
	ErrTooManyAttempts       = errors.New("too many attempts, please try again later")
	ErrUnauthorized          = errors.New("unauthorized")
	ErrAdminRequired         = errors.New("admin role required")
	ErrCannotDeleteSelf      = errors.New("cannot delete own account")
	ErrCannotDeleteLastAdmin = errors.New("cannot delete last admin user")
)

// dummyPasswordHash is a pre-computed valid Argon2id hash used to prevent timing
// side-channels when authenticating non-existent usernames.
const dummyPasswordHash = "$argon2id$v=19$m=65536,t=3,p=2$OcmZvIua+ASMMohB+hBYvw$XWtmVOa87Y7yBi7/JOhrLJM+fDFSufieY7suNIUljKY"

// Service coordinates authentication, session management, password hashing, and rate limiting.
type Service struct {
	store        *store.Store
	rateLimiter  *RateLimiter
	trustedProxy string
	publicURL    string

	mu        sync.RWMutex
	setupCode string
}

// NewService creates a new auth Service, generating a first-run setup code if no users exist.
func NewService(st *store.Store, trustedProxy, publicURL string) (*Service, error) {
	s := &Service{
		store:        st,
		rateLimiter:  NewRateLimiter(),
		trustedProxy: trustedProxy,
		publicURL:    publicURL,
	}

	count, err := st.CountUsers(context.Background())
	if err != nil {
		return nil, fmt.Errorf("check initial users count: %w", err)
	}

	if count == 0 {
		s.setupCode = generateSetupCode()
	}

	return s, nil
}

func generateSetupCode() string {
	n1, _ := rand.Int(rand.Reader, big.NewInt(1000))
	n2, _ := rand.Int(rand.Reader, big.NewInt(1000))
	return fmt.Sprintf("%03d-%03d", n1.Int64(), n2.Int64())
}

// SetupCode returns the first-run setup code, or "" if setup is already complete.
func (s *Service) SetupCode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.setupCode
}

// SetupNeeded checks if any users exist in the database.
func (s *Service) SetupNeeded(ctx context.Context) (bool, error) {
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// VerifySetupCode compares an input code with the active setup code (ignoring hyphens).
func (s *Service) VerifySetupCode(input string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.setupCode == "" {
		return false
	}

	cleanInput := strings.ReplaceAll(strings.TrimSpace(input), "-", "")
	cleanStored := strings.ReplaceAll(s.setupCode, "-", "")

	if len(cleanInput) != len(cleanStored) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(cleanInput), []byte(cleanStored)) == 1
}

// ClearSetupCode clears the setup code once the initial admin is registered.
func (s *Service) ClearSetupCode() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setupCode = ""
}

// Store returns the underlying store.Store.
func (s *Service) Store() *store.Store {
	return s.store
}

// RateLimiter returns the RateLimiter instance.
func (s *Service) RateLimiter() *RateLimiter {
	return s.rateLimiter
}

// TrustedProxy returns the configured trusted proxy IP or hostname.
func (s *Service) TrustedProxy() string {
	return s.trustedProxy
}

// PublicURL returns the configured public URL.
func (s *Service) PublicURL() string {
	return s.publicURL
}

// Setup creates the initial admin user and returns a new session.
func (s *Service) Setup(ctx context.Context, setupCode, username, displayName, password, userAgent, ip string) (*store.User, string, time.Time, error) {
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if !s.rateLimiter.SetupCodeAllowed(ip) {
		return nil, "", time.Time{}, ErrTooManyAttempts
	}

	needed, err := s.SetupNeeded(ctx)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if !needed {
		return nil, "", time.Time{}, ErrSetupNotNeeded
	}

	if !s.VerifySetupCode(setupCode) {
		s.rateLimiter.RecordSetupFailure(ip)
		return nil, "", time.Time{}, ErrInvalidSetupCode
	}

	s.rateLimiter.ResetSetupFailures(ip)

	if err := ValidateUsername(username); err != nil {
		return nil, "", time.Time{}, err
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = username
	}

	passHash, err := HashPassword(password)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return nil, "", time.Time{}, ErrBusy
		}
		return nil, "", time.Time{}, err
	}

	u := &store.User{
		Username:    username,
		DisplayName: displayName,
		Role:        "admin",
		PassHash:    passHash,
		MustChange:  false,
	}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, "", time.Time{}, err
	}

	s.ClearSetupCode()

	token, hash, err := GenerateSessionToken()
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}

	expiresAt := time.Now().Add(SessionDuration)
	sess := &store.Session{
		IDHash:    hash,
		UserID:    u.ID,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: expiresAt.Unix(),
		UserAgent: userAgent,
		IP:        ip,
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("create session: %w", err)
	}

	return u, token, expiresAt, nil
}

// Login validates user credentials and establishes a new session.
func (s *Service) Login(ctx context.Context, username, password, userAgent, ip string) (*store.User, string, time.Time, error) {
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if !s.rateLimiter.LoginAllowed(username, ip) {
		return nil, "", time.Time{}, ErrInvalidCredentials
	}

	u, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		_, vErr := VerifyPassword(password, dummyPasswordHash)
		if errors.Is(vErr, ErrBusy) {
			return nil, "", time.Time{}, ErrBusy
		}
		s.rateLimiter.RecordLoginFailure(username, ip)
		return nil, "", time.Time{}, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(password, u.PassHash)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return nil, "", time.Time{}, ErrBusy
		}
		s.rateLimiter.RecordLoginFailure(username, ip)
		return nil, "", time.Time{}, ErrInvalidCredentials
	}
	if !ok {
		s.rateLimiter.RecordLoginFailure(username, ip)
		return nil, "", time.Time{}, ErrInvalidCredentials
	}

	s.rateLimiter.ResetLoginFailures(username, ip)

	token, hash, err := GenerateSessionToken()
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}

	expiresAt := time.Now().Add(SessionDuration)
	sess := &store.Session{
		IDHash:    hash,
		UserID:    u.ID,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: expiresAt.Unix(),
		UserAgent: userAgent,
		IP:        ip,
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("create session: %w", err)
	}

	return u, token, expiresAt, nil
}

// AuthenticateSession verifies a session token and returns the corresponding user and session.
// It also slides the session expiration.
func (s *Service) AuthenticateSession(ctx context.Context, idHash [32]byte) (*store.User, *store.Session, error) {
	sess, u, err := s.store.GetSession(ctx, idHash)
	if err != nil {
		return nil, nil, ErrUnauthorized
	}

	// Slide expiration
	newExpires := time.Now().Add(SessionDuration).Unix()
	_ = s.store.TouchSession(ctx, idHash, newExpires)
	sess.ExpiresAt = newExpires

	return u, sess, nil
}

// Logout revokes a session by its token hash.
func (s *Service) Logout(ctx context.Context, idHash [32]byte) error {
	return s.store.DeleteSession(ctx, idHash)
}

// ChangePassword updates a user's password and revokes all other sessions.
func (s *Service) ChangePassword(ctx context.Context, userID int64, currentPass, newPass string, currentTokenHash [32]byte) error {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return ErrUnauthorized
	}

	ok, err := VerifyPassword(currentPass, u.PassHash)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return ErrBusy
		}
		return errors.New("current password incorrect")
	}
	if !ok {
		return errors.New("current password incorrect")
	}

	newHash, err := HashPassword(newPass)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return ErrBusy
		}
		return err
	}

	if err := s.store.UpdateUserPassword(ctx, userID, newHash, false); err != nil {
		return err
	}

	// Revoke other sessions
	return s.store.DeleteSessionsForUserExcept(ctx, userID, &currentTokenHash)
}
