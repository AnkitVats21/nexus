package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"nexus/pkg/store"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	defaultBaseURL = "https://fcm.googleapis.com"
	fcmScope       = "https://www.googleapis.com/auth/firebase.messaging"
)

var (
	ErrNoPushToken = errors.New("no push token registered for device")
)

// TokenStore describes the store operations required by the push sender.
type TokenStore interface {
	GetPushToken(ctx context.Context, deviceID string) (*store.PushToken, error)
	DeletePushToken(ctx context.Context, deviceID string) error
	MarkPushOK(ctx context.Context, deviceID string) error
	MarkPushFail(ctx context.Context, deviceID string) error
}

// Sender sends data-only push notifications to edge devices.
type Sender interface {
	Send(ctx context.Context, deviceID string, msg Message) error
}

// Option configures an FCMSender.
type Option func(*FCMSender)

// WithBaseURL overrides the base FCM API endpoint (useful for httptest servers).
func WithBaseURL(url string) Option {
	return func(s *FCMSender) {
		s.baseURL = strings.TrimRight(url, "/")
	}
}

// WithBackoffs configures the retry backoff sequence.
func WithBackoffs(backoffs []time.Duration) Option {
	return func(s *FCMSender) {
		s.backoffs = backoffs
	}
}

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(client *http.Client) Option {
	return func(s *FCMSender) {
		s.httpClient = client
	}
}

// WithLogger overrides the logger.
func WithLogger(log *slog.Logger) Option {
	return func(s *FCMSender) {
		s.log = log
	}
}

// WithSleepFn overrides the sleep provider for testing.
func WithSleepFn(fn func(time.Duration) <-chan time.Time) Option {
	return func(s *FCMSender) {
		s.sleepFn = fn
	}
}

// FCMSender sends push messages using FCM HTTP v1.
type FCMSender struct {
	project     string
	tokenSource oauth2.TokenSource
	store       TokenStore
	baseURL     string
	httpClient  *http.Client
	backoffs    []time.Duration
	log         *slog.Logger
	sleepFn     func(time.Duration) <-chan time.Time
}

// NewFCMSender creates an FCMSender with a given OAuth2 TokenSource.
func NewFCMSender(project string, ts oauth2.TokenSource, st TokenStore, opts ...Option) *FCMSender {
	s := &FCMSender{
		project:     project,
		tokenSource: ts,
		store:       st,
		baseURL:     defaultBaseURL,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		backoffs:    []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second},
		log:         slog.Default(),
		sleepFn:     time.After,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// NewFCMSenderFromCredentials creates an FCMSender from service account JSON bytes.
func NewFCMSenderFromCredentials(project string, credsJSON []byte, st TokenStore, opts ...Option) (*FCMSender, error) {
	jwtCfg, err := google.JWTConfigFromJSON(credsJSON, fcmScope)
	if err != nil {
		return nil, fmt.Errorf("parse fcm service account json: %w", err)
	}
	ts := jwtCfg.TokenSource(context.Background())
	return NewFCMSender(project, ts, st, opts...), nil
}

// NewFCMSenderFromCredentialsFile creates an FCMSender from a service account JSON file.
func NewFCMSenderFromCredentialsFile(project string, credsPath string, st TokenStore, opts ...Option) (*FCMSender, error) {
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return nil, fmt.Errorf("read fcm credentials file %s: %w", credsPath, err)
	}
	return NewFCMSenderFromCredentials(project, data, st, opts...)
}

type fcmRequestBody struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android AndroidConfig     `json:"android"`
}

type fcmErrorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type      string `json:"@type"`
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

// Send delivers a push message to deviceID via FCM HTTP v1.
//
// Following FIREBASE.md §4.3 error rules:
// - UNREGISTERED, or INVALID_ARGUMENT about the token: delete the token.
// - UNAVAILABLE, INTERNAL, 429: retry with backoff (0.5 s, 1 s, 2 s; give up after caller deadline).
// - QUOTA_EXCEEDED: log it and don't retry.
// - Update last_ok_at and fail_count.
// - Never log tokens; log device ids.
func (s *FCMSender) Send(ctx context.Context, deviceID string, msg Message) error {
	pt, err := s.store.GetPushToken(ctx, deviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNoPushToken
		}
		return fmt.Errorf("lookup push token: %w", err)
	}
	if pt == nil || pt.Token == "" {
		return ErrNoPushToken
	}

	data, android, err := msg.BuildPayload()
	if err != nil {
		return fmt.Errorf("build push payload: %w", err)
	}

	body := fcmRequestBody{
		Message: fcmMessage{
			Token:   pt.Token,
			Data:    data,
			Android: android,
		},
	}
	jsonBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal push request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", s.baseURL, s.project)

	storeCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 5*time.Second)
	}

	var lastErr error
	for attempt := 0; attempt <= len(s.backoffs); attempt++ {
		if attempt > 0 {
			backoff := s.backoffs[attempt-1]
			select {
			case <-ctx.Done():
				cCtx, cancel := storeCtx()
				_ = s.store.MarkPushFail(cCtx, deviceID)
				cancel()
				return fmt.Errorf("push context canceled: %w", ctx.Err())
			case <-s.sleepFn(backoff):
			}
		}

		err, retryable, deleteToken, quotaExceeded := s.executeSend(ctx, deviceID, url, jsonBytes)
		if err == nil {
			cCtx, cancel := storeCtx()
			_ = s.store.MarkPushOK(cCtx, deviceID)
			cancel()
			return nil
		}

		lastErr = err

		if deleteToken {
			cCtx, cancel := storeCtx()
			_ = s.store.DeletePushToken(cCtx, deviceID)
			cancel()
			return err
		}

		if quotaExceeded {
			s.log.Error("FCM quota exceeded", "device_id", deviceID)
			cCtx, cancel := storeCtx()
			_ = s.store.MarkPushFail(cCtx, deviceID)
			cancel()
			return err
		}

		if !retryable {
			cCtx, cancel := storeCtx()
			_ = s.store.MarkPushFail(cCtx, deviceID)
			cancel()
			return err
		}
	}

	cCtx, cancel := storeCtx()
	_ = s.store.MarkPushFail(cCtx, deviceID)
	cancel()
	return fmt.Errorf("push failed after retries: %w", lastErr)
}

func (s *FCMSender) executeSend(ctx context.Context, deviceID string, url string, reqBody []byte) (err error, retryable bool, deleteToken bool, quotaExceeded bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create push request: %w", err), false, false, false
	}
	req.Header.Set("Content-Type", "application/json")

	// Fetch OAuth2 token
	tok, err := s.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("obtain oauth token: %w", err), false, false, false
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Transport errors (e.g. connection refused or timeout) can be retried if context is still valid
		if ctx.Err() != nil {
			return ctx.Err(), false, false, false
		}
		return fmt.Errorf("http request failed: %w", err), true, false, false
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil, false, false, false
	}

	var errResp fcmErrorResponse
	_ = json.Unmarshal(respBytes, &errResp)

	errStatus := errResp.Error.Status
	errMsg := errResp.Error.Message
	errDesc := errMsg
	if errDesc == "" {
		errDesc = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var errorCodes []string
	for _, d := range errResp.Error.Details {
		if d.ErrorCode != "" {
			errorCodes = append(errorCodes, d.ErrorCode)
		}
	}

	hasCode := func(target string) bool {
		for _, c := range errorCodes {
			if strings.EqualFold(c, target) {
				return true
			}
		}
		return strings.EqualFold(errStatus, target) || strings.Contains(strings.ToUpper(errMsg), target)
	}

	// 1. UNREGISTERED or INVALID_ARGUMENT -> delete token, no retry
	if hasCode("UNREGISTERED") || (resp.StatusCode == http.StatusNotFound && hasCode("NOT_FOUND")) {
		return fmt.Errorf("fcm token unregistered: %s", errDesc), false, true, false
	}
	if hasCode("INVALID_ARGUMENT") {
		return fmt.Errorf("fcm invalid argument: %s", errDesc), false, true, false
	}

	// 2. QUOTA_EXCEEDED -> no retry
	if hasCode("QUOTA_EXCEEDED") || (errStatus == "RESOURCE_EXHAUSTED" && resp.StatusCode != http.StatusTooManyRequests) {
		return fmt.Errorf("fcm quota exceeded: %s", errDesc), false, false, true
	}

	// 3. UNAVAILABLE, INTERNAL, 429 -> retryable
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusInternalServerError {
		return fmt.Errorf("fcm retryable error (status %d): %s", resp.StatusCode, errDesc), true, false, false
	}
	if hasCode("UNAVAILABLE") || hasCode("INTERNAL") {
		return fmt.Errorf("fcm unavailable/internal: %s", errDesc), true, false, false
	}

	// Other errors
	return fmt.Errorf("fcm error (status %d): %s", resp.StatusCode, errDesc), false, false, false
}
