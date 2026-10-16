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
	"strconv"
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
	DeletePushTokenIf(ctx context.Context, deviceID string, token string) error
	MarkPushOK(ctx context.Context, deviceID string) error
	MarkPushOKIf(ctx context.Context, deviceID string, token string) error
	MarkPushFail(ctx context.Context, deviceID string) error
	MarkPushFailIf(ctx context.Context, deviceID string, token string) error
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
// Uses an http.Client with a timeout for OAuth token fetching.
func NewFCMSenderFromCredentials(project string, credsJSON []byte, st TokenStore, opts ...Option) (*FCMSender, error) {
	jwtCfg, err := google.JWTConfigFromJSON(credsJSON, fcmScope)
	if err != nil {
		return nil, fmt.Errorf("parse fcm service account json: %w", err)
	}
	oauthClient := &http.Client{Timeout: 10 * time.Second}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, oauthClient)
	ts := jwtCfg.TokenSource(ctx)
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
			Type            string `json:"@type"`
			ErrorCode       string `json:"errorCode"`
			FieldViolations []struct {
				Field       string `json:"field"`
				Description string `json:"description"`
			} `json:"fieldViolations"`
		} `json:"details"`
	} `json:"error"`
}

// fetchToken fetches an OAuth2 token bounded by ctx so hung endpoints return at deadline.
func (s *FCMSender) fetchToken(ctx context.Context) (*oauth2.Token, error) {
	type tokenRes struct {
		tok *oauth2.Token
		err error
	}
	ch := make(chan tokenRes, 1)
	go func() {
		tok, err := s.tokenSource.Token()
		ch <- tokenRes{tok: tok, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.tok, res.err
	}
}

// Send delivers a push message to deviceID via FCM HTTP v1.
//
// Following FIREBASE.md §4.3 and task-036 review rules:
// - Delete token only on UNREGISTERED or INVALID_ARGUMENT about message.token.
// - Conditional deletion (DeletePushTokenIf) ensures no race deletes a freshly uploaded token.
// - UNAVAILABLE, INTERNAL, 429, and OAuth token fetch errors retry with backoff.
// - Retry-After on 429 is respected within ctx.
// - QUOTA_EXCEEDED: log it and don't retry.
// - Update last_ok_at and fail_count conditionally by token.
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

	sentToken := pt.Token

	data, android, err := msg.BuildPayload()
	if err != nil {
		return fmt.Errorf("build push payload: %w", err)
	}

	body := fcmRequestBody{
		Message: fcmMessage{
			Token:   sentToken,
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
	var nextBackoff time.Duration

	for attempt := 0; attempt <= len(s.backoffs); attempt++ {
		if attempt > 0 {
			backoff := nextBackoff
			select {
			case <-ctx.Done():
				cCtx, cancel := storeCtx()
				_ = s.store.MarkPushFailIf(cCtx, deviceID, sentToken)
				cancel()
				return fmt.Errorf("push context canceled: %w", ctx.Err())
			case <-s.sleepFn(backoff):
			}
		}

		err, retryable, deleteToken, quotaExceeded, retryAfter := s.executeSend(ctx, deviceID, url, jsonBytes)
		if err == nil {
			cCtx, cancel := storeCtx()
			_ = s.store.MarkPushOKIf(cCtx, deviceID, sentToken)
			cancel()
			return nil
		}

		lastErr = err

		if deleteToken {
			cCtx, cancel := storeCtx()
			_ = s.store.DeletePushTokenIf(cCtx, deviceID, sentToken)
			cancel()
			return err
		}

		if quotaExceeded {
			s.log.Error("FCM quota exceeded", "device_id", deviceID)
			cCtx, cancel := storeCtx()
			_ = s.store.MarkPushFailIf(cCtx, deviceID, sentToken)
			cancel()
			return err
		}

		if !retryable {
			cCtx, cancel := storeCtx()
			_ = s.store.MarkPushFailIf(cCtx, deviceID, sentToken)
			cancel()
			return err
		}

		if attempt < len(s.backoffs) {
			nextBackoff = s.backoffs[attempt]
			if retryAfter > nextBackoff {
				nextBackoff = retryAfter
			}
		}
	}

	cCtx, cancel := storeCtx()
	_ = s.store.MarkPushFailIf(cCtx, deviceID, sentToken)
	cancel()
	return fmt.Errorf("push failed after retries: %w", lastErr)
}

func (s *FCMSender) executeSend(ctx context.Context, deviceID string, url string, reqBody []byte) (err error, retryable bool, deleteToken bool, quotaExceeded bool, retryAfter time.Duration) {
	tok, err := s.fetchToken(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return err, false, false, false, 0
		}
		// Token-fetch errors are retryable like UNAVAILABLE
		return fmt.Errorf("fetch oauth token: %w", err), true, false, false, 0
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create push request: %w", err), false, false, false, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err(), false, false, false, 0
		}
		return fmt.Errorf("http request failed: %w", err), true, false, false, 0
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil, false, false, false, 0
	}

	var errResp fcmErrorResponse
	_ = json.Unmarshal(respBytes, &errResp)

	errStatus := errResp.Error.Status
	errMsg := errResp.Error.Message
	errDesc := errMsg
	if errDesc == "" {
		errDesc = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var hasUnregistered bool
	var hasTokenFieldViolation bool
	var hasQuotaExceeded bool
	var hasUnavailable bool
	var hasInternal bool

	for _, d := range errResp.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			hasUnregistered = true
		}
		if d.ErrorCode == "QUOTA_EXCEEDED" {
			hasQuotaExceeded = true
		}
		if d.ErrorCode == "UNAVAILABLE" {
			hasUnavailable = true
		}
		if d.ErrorCode == "INTERNAL" {
			hasInternal = true
		}
		for _, fv := range d.FieldViolations {
			if fv.Field == "message.token" {
				hasTokenFieldViolation = true
			}
		}
	}

	// 1. Delete token ONLY on UNREGISTERED or INVALID_ARGUMENT about message.token
	if hasUnregistered {
		return fmt.Errorf("fcm token unregistered: %s", errDesc), false, true, false, 0
	}
	if errStatus == "INVALID_ARGUMENT" && hasTokenFieldViolation {
		return fmt.Errorf("fcm invalid token argument: %s", errDesc), false, true, false, 0
	}

	// 2. Other INVALID_ARGUMENT (e.g. payload issue): no delete, no retry, MarkPushFail, log status and message (never token)
	if errStatus == "INVALID_ARGUMENT" {
		s.log.Warn("FCM payload invalid argument", "device_id", deviceID, "status", errStatus, "message", errMsg)
		return fmt.Errorf("fcm invalid argument: %s", errDesc), false, false, false, 0
	}

	// 3. 404 NOT_FOUND without UNREGISTERED: no delete, no retry, MarkPushFail
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("fcm not found: %s", errDesc), false, false, false, 0
	}

	// 4. QUOTA_EXCEEDED: log it, no retry
	if hasQuotaExceeded || (errStatus == "RESOURCE_EXHAUSTED" && resp.StatusCode != http.StatusTooManyRequests) {
		return fmt.Errorf("fcm quota exceeded: %s", errDesc), false, false, true, 0
	}

	// 5. UNAVAILABLE, INTERNAL, 429: retryable
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusInternalServerError ||
		errStatus == "UNAVAILABLE" || errStatus == "INTERNAL" || hasUnavailable || hasInternal {
		if resp.StatusCode == http.StatusTooManyRequests {
			if raStr := resp.Header.Get("Retry-After"); raStr != "" {
				if secs, parseErr := strconv.Atoi(strings.TrimSpace(raStr)); parseErr == nil && secs > 0 {
					retryAfter = time.Duration(secs) * time.Second
				}
			}
		}
		return fmt.Errorf("fcm retryable error (status %d): %s", resp.StatusCode, errDesc), true, false, false, retryAfter
	}

	// Other errors: no delete, no retry
	return fmt.Errorf("fcm error (status %d): %s", resp.StatusCode, errDesc), false, false, false, 0
}
