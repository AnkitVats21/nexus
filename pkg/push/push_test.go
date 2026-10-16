package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"nexus/pkg/store"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func newTestStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "nexus-push-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.Open(dbPath)
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

func setupDeviceAndToken(t *testing.T, s *store.Store, devID, token string) {
	t.Helper()
	ctx := context.Background()
	_ = s.CreateUser(ctx, &store.User{
		Username:    "user-" + devID,
		DisplayName: "Test User",
		Role:        "member",
		PassHash:    "dummy",
	})
	u, _ := s.GetUserByUsername(ctx, "user-"+devID)
	_ = s.UpsertDevice(ctx, &store.Device{
		ID:      devID,
		OwnerID: u.ID,
		Name:    "Phone",
		Kind:    "android",
	})
	_ = s.UpsertPushToken(ctx, &store.PushToken{
		DeviceID: devID,
		Provider: "fcm",
		Token:    token,
	})
}

func TestMessagePayloads(t *testing.T) {
	// 1. Call
	mCall := Message{Type: TypeCall, CallID: "call-100"}
	data, android, err := mCall.BuildPayload()
	if err != nil {
		t.Fatalf("BuildPayload call: %v", err)
	}
	if data["t"] != "call" || data["call_id"] != "call-100" || data["v"] != "1" {
		t.Fatalf("unexpected call data: %+v", data)
	}
	if android.Priority != "HIGH" || android.TTL != "30s" || android.CollapseKey != "call-100" {
		t.Fatalf("unexpected call android: %+v", android)
	}

	// 2. Call End
	mEnd := Message{Type: TypeCallEnd, CallID: "call-100", Reason: "answered_elsewhere"}
	data, android, err = mEnd.BuildPayload()
	if err != nil {
		t.Fatalf("BuildPayload call_end: %v", err)
	}
	if data["t"] != "call_end" || data["call_id"] != "call-100" || data["reason"] != "answered_elsewhere" || data["v"] != "1" {
		t.Fatalf("unexpected call_end data: %+v", data)
	}
	if android.Priority != "NORMAL" || android.TTL != "60s" || android.CollapseKey != "call-100" {
		t.Fatalf("unexpected call_end android: %+v", android)
	}

	// 3. Missed
	mMissed := Message{Type: TypeMissed, CallID: "call-100"}
	data, android, err = mMissed.BuildPayload()
	if err != nil {
		t.Fatalf("BuildPayload missed: %v", err)
	}
	if data["t"] != "missed" || data["call_id"] != "call-100" || data["v"] != "1" {
		t.Fatalf("unexpected missed data: %+v", data)
	}
	if android.Priority != "NORMAL" || android.TTL != "86400s" || android.CollapseKey != "call-100" {
		t.Fatalf("unexpected missed android: %+v", android)
	}

	// 4. Voice message
	mVoice := Message{Type: TypeVoiceMessage, MessageID: "msg-200"}
	data, android, err = mVoice.BuildPayload()
	if err != nil {
		t.Fatalf("BuildPayload voice_message: %v", err)
	}
	if data["t"] != "voice_message" || data["message_id"] != "msg-200" || data["v"] != "1" {
		t.Fatalf("unexpected voice_message data: %+v", data)
	}
	if android.Priority != "NORMAL" || android.TTL != "86400s" || android.CollapseKey != "" {
		t.Fatalf("unexpected voice_message android: %+v", android)
	}

	// 5. Missing required fields
	if _, _, err := (Message{Type: TypeCall}).BuildPayload(); err == nil {
		t.Fatalf("expected error for call message without call_id")
	}
	if _, _, err := (Message{Type: TypeCallEnd}).BuildPayload(); err == nil {
		t.Fatalf("expected error for call_end message without call_id")
	}
	if _, _, err := (Message{Type: TypeMissed}).BuildPayload(); err == nil {
		t.Fatalf("expected error for missed message without call_id")
	}
	if _, _, err := (Message{Type: TypeVoiceMessage}).BuildPayload(); err == nil {
		t.Fatalf("expected error for voice_message without message_id")
	}

	// 6. Unknown message type must be rejected
	if _, _, err := (Message{Type: "unknown_type", CallID: "c-1"}).BuildPayload(); err == nil {
		t.Fatalf("expected error for unknown message type")
	}
}

func TestFCMSenderSuccessAndRequestShape(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-user-1"
	pushToken := "fcm-registration-token-999"
	setupDeviceAndToken(t, st, devID, pushToken)

	var receivedAuth string
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/my-test-proj/messages:send" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		receivedAuth = r.Header.Get("Authorization")
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"projects/my-test-proj/messages/msg-123"}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake-oauth-bearer-token"})
	sender := NewFCMSender("my-test-proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	msg := Message{Type: TypeCall, CallID: "call-xyz"}
	if err := sender.Send(ctx, devID, msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	expectedAuth := "Bearer fake-oauth-bearer-token"
	if receivedAuth != expectedAuth {
		t.Fatalf("expected Authorization %q, got %q", expectedAuth, receivedAuth)
	}

	msgObj, ok := receivedBody["message"].(map[string]any)
	if !ok {
		t.Fatalf("expected message object in body: %+v", receivedBody)
	}
	if msgObj["token"] != pushToken {
		t.Fatalf("expected token %q, got %v", pushToken, msgObj["token"])
	}
	if _, hasNotif := msgObj["notification"]; hasNotif {
		t.Fatalf("payload must NOT have notification block")
	}

	dataObj, ok := msgObj["data"].(map[string]any)
	if !ok || dataObj["t"] != "call" || dataObj["call_id"] != "call-xyz" || dataObj["v"] != "1" {
		t.Fatalf("unexpected data object: %+v", dataObj)
	}

	androidObj, ok := msgObj["android"].(map[string]any)
	if !ok || androidObj["priority"] != "HIGH" || androidObj["ttl"] != "30s" || androidObj["collapse_key"] != "call-xyz" {
		t.Fatalf("unexpected android object: %+v", androidObj)
	}

	pt, err := st.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken: %v", err)
	}
	if pt.LastOKAt == 0 || pt.FailCount != 0 {
		t.Fatalf("expected LastOKAt > 0 and FailCount == 0, got %+v", pt)
	}
}

func TestFCMSenderMissingToken(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st)

	ctx := context.Background()
	err := sender.Send(ctx, "dev-no-token", Message{Type: TypeCall, CallID: "call-1"})
	if err != ErrNoPushToken {
		t.Fatalf("expected ErrNoPushToken, got %v", err)
	}
}

func TestFCMSenderErrorUnregisteredDeletesToken(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-unreg"
	setupDeviceAndToken(t, st, devID, "bad-token-1")

	var requestsCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": 404,
				"message": "Requested entity was not found.",
				"status": "NOT_FOUND",
				"details": [
					{
						"@type": "type.googleapis.com/google.firebase.fcm.v1.FcmError",
						"errorCode": "UNREGISTERED"
					}
				]
			}
		}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err == nil {
		t.Fatalf("expected error for UNREGISTERED, got nil")
	}

	if count := atomic.LoadInt32(&requestsCount); count != 1 {
		t.Fatalf("expected 1 request (no retry), got %d", count)
	}

	if _, err := st.GetPushToken(ctx, devID); err != store.ErrNotFound {
		t.Fatalf("expected token to be deleted on UNREGISTERED, got %v", err)
	}
}

func TestFCMSenderErrorNotFoundWithoutUnregisteredKeepsToken(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-notfound-keeps"
	tokenVal := "valid-token-wrong-project"
	setupDeviceAndToken(t, st, devID, tokenVal)

	var requestsCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": 404,
				"message": "Requested entity was not found.",
				"status": "NOT_FOUND"
			}
		}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err == nil {
		t.Fatalf("expected error for 404 NOT_FOUND, got nil")
	}

	// Must not retry
	if count := atomic.LoadInt32(&requestsCount); count != 1 {
		t.Fatalf("expected 1 request (no retry), got %d", count)
	}

	// Token must NOT be deleted
	pt, err := st.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("token was unexpectedly deleted on 404 NOT_FOUND without UNREGISTERED: %v", err)
	}
	if pt.Token != tokenVal {
		t.Fatalf("token mismatch: %+v", pt)
	}
	if pt.FailCount != 1 {
		t.Fatalf("expected FailCount == 1, got %d", pt.FailCount)
	}
}

func TestFCMSenderErrorTokenInvalidArgumentDeletesToken(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-token-invalid-arg"
	setupDeviceAndToken(t, st, devID, "malformed-fcm-token")

	var requestsCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": 400,
				"message": "The registration token is not a valid FCM registration token",
				"status": "INVALID_ARGUMENT",
				"details": [
					{
						"@type": "type.googleapis.com/google.rpc.BadRequest",
						"fieldViolations": [
							{
								"field": "message.token",
								"description": "Invalid registration token"
							}
						]
					}
				]
			}
		}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err == nil {
		t.Fatalf("expected error for token INVALID_ARGUMENT, got nil")
	}

	if count := atomic.LoadInt32(&requestsCount); count != 1 {
		t.Fatalf("expected 1 request (no retry), got %d", count)
	}

	// Token must be deleted from store
	if _, err := st.GetPushToken(ctx, devID); err != store.ErrNotFound {
		t.Fatalf("expected token to be deleted on fieldViolation message.token, got %v", err)
	}
}

func TestFCMSenderErrorPayloadInvalidArgumentKeepsToken(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-payload-invalid-arg"
	tokenVal := "good-token-bad-payload"
	setupDeviceAndToken(t, st, devID, tokenVal)

	var requestsCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": 400,
				"message": "Invalid TTL field duration format",
				"status": "INVALID_ARGUMENT",
				"details": [
					{
						"@type": "type.googleapis.com/google.rpc.BadRequest",
						"fieldViolations": [
							{
								"field": "message.android.ttl",
								"description": "Invalid duration format"
							}
						]
					}
				]
			}
		}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err == nil {
		t.Fatalf("expected error for payload INVALID_ARGUMENT, got nil")
	}

	if count := atomic.LoadInt32(&requestsCount); count != 1 {
		t.Fatalf("expected 1 request (no retry), got %d", count)
	}

	// Token must NOT be deleted for payload errors
	pt, err := st.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("token was unexpectedly deleted on payload INVALID_ARGUMENT: %v", err)
	}
	if pt.Token != tokenVal {
		t.Fatalf("token mismatch: %+v", pt)
	}
	if pt.FailCount != 1 {
		t.Fatalf("expected FailCount == 1, got %d", pt.FailCount)
	}
}

func TestFCMSenderConditionalDeleteRace(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-race"
	tokenA := "token-initial-A"
	tokenB := "token-updated-B"
	setupDeviceAndToken(t, st, devID, tokenA)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate race: while FCM call for token A is being processed, device uploads token B!
		_ = st.UpsertPushToken(context.Background(), &store.PushToken{
			DeviceID: devID,
			Provider: "fcm",
			Token:    tokenB,
		})

		// Now return UNREGISTERED for the request that used token A
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": 404,
				"status": "NOT_FOUND",
				"details": [{"@type": "type.googleapis.com/google.firebase.fcm.v1.FcmError", "errorCode": "UNREGISTERED"}]
			}
		}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	_ = sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "c1"})

	// Token B must have survived!
	surviving, err := st.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("expected tokenB to survive race deletion, got err: %v", err)
	}
	if surviving.Token != tokenB {
		t.Fatalf("expected tokenB to survive, got: %s", surviving.Token)
	}
}

func TestFCMSenderOAuthFetchBoundedByDeadline(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-oauth-hang"
	setupDeviceAndToken(t, st, devID, "token-1")

	// TokenSource that blocks forever
	blockingTS := &blockingTokenSource{}
	sender := NewFCMSender("proj", blockingTS, st,
		WithBackoffs([]time.Duration{time.Millisecond}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "c1"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected context deadline error, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Send took too long to return (%v), failed to observe context deadline", elapsed)
	}
}

type blockingTokenSource struct{}

func (b *blockingTokenSource) Token() (*oauth2.Token, error) {
	select {}
}

func TestFCMSenderOAuthFetchRetryOnFailure(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-oauth-retry"
	setupDeviceAndToken(t, st, devID, "token-1")

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	flakyTS := &flakyTokenSource{failCount: 1}
	sender := NewFCMSender("proj", flakyTS, st,
		WithBaseURL(srv.URL),
		WithBackoffs([]time.Duration{time.Millisecond, time.Millisecond}),
	)

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "c1"})
	if err != nil {
		t.Fatalf("expected send to succeed on oauth retry, got: %v", err)
	}
	_ = attempts
}

type flakyTokenSource struct {
	calls     int32
	failCount int32
}

func (f *flakyTokenSource) Token() (*oauth2.Token, error) {
	call := atomic.AddInt32(&f.calls, 1)
	if call <= f.failCount {
		return nil, errors.New("transient oauth token server 503")
	}
	return &oauth2.Token{AccessToken: "good-token"}, nil
}

func TestFCMSenderRetryAfterHeader(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-retry-after"
	setupDeviceAndToken(t, st, devID, "token-1")

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		if att == 1 {
			w.Header().Set("Retry-After", "1") // 1 second
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	var sleepCalls []time.Duration
	mockSleep := func(d time.Duration) <-chan time.Time {
		sleepCalls = append(sleepCalls, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st,
		WithBaseURL(srv.URL),
		WithBackoffs([]time.Duration{100 * time.Millisecond, 200 * time.Millisecond}),
		WithSleepFn(mockSleep),
	)

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "c1"})
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if len(sleepCalls) != 1 {
		t.Fatalf("expected 1 sleep call, got %d", len(sleepCalls))
	}
	// The sleep duration should be 1 second (from Retry-After), not 100ms
	if sleepCalls[0] != 1*time.Second {
		t.Fatalf("expected sleep to honor Retry-After (1s), got %v", sleepCalls[0])
	}
}

func TestFCMSenderErrorQuotaExceededNoRetry(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-quota"
	setupDeviceAndToken(t, st, devID, "quota-token")

	var requestsCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestsCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": 429,
				"message": "Quota exceeded",
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{
						"@type": "type.googleapis.com/google.firebase.fcm.v1.FcmError",
						"errorCode": "QUOTA_EXCEEDED"
					}
				]
			}
		}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st, WithBaseURL(srv.URL))

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err == nil {
		t.Fatalf("expected error for QUOTA_EXCEEDED, got nil")
	}

	if count := atomic.LoadInt32(&requestsCount); count != 1 {
		t.Fatalf("expected 1 request (no retry for QUOTA_EXCEEDED), got %d", count)
	}

	pt, err := st.GetPushToken(ctx, devID)
	if err != nil {
		t.Fatalf("GetPushToken: %v", err)
	}
	if pt.FailCount != 1 {
		t.Fatalf("expected FailCount == 1, got %d", pt.FailCount)
	}
}

func TestFCMSenderRetryBranchesSuccess(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-retry-ok"
	setupDeviceAndToken(t, st, devID, "retry-token")

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		if att == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 UNAVAILABLE
			_, _ = w.Write([]byte(`{"error":{"code":503,"status":"UNAVAILABLE"}}`))
			return
		}
		if att == 2 {
			w.WriteHeader(http.StatusTooManyRequests) // 429
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Rate limit"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st,
		WithBaseURL(srv.URL),
		WithBackoffs([]time.Duration{time.Millisecond, 2 * time.Millisecond}),
	)

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err != nil {
		t.Fatalf("expected send to succeed on 3rd attempt, got %v", err)
	}

	if count := atomic.LoadInt32(&attempts); count != 3 {
		t.Fatalf("expected 3 attempts, got %d", count)
	}

	pt, err := st.GetPushToken(ctx, devID)
	if err != nil || pt.LastOKAt == 0 || pt.FailCount != 0 {
		t.Fatalf("expected MarkPushOK on success, got %+v, err=%v", pt, err)
	}
}

func TestFCMSenderRetriesExhausted(t *testing.T) {
	st, cleanup := newTestStore(t)
	defer cleanup()

	devID := "phone-retry-fail"
	setupDeviceAndToken(t, st, devID, "retry-token")

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError) // 500 INTERNAL
		_, _ = w.Write([]byte(`{"error":{"code":500,"status":"INTERNAL"}}`))
	}))
	defer srv.Close()

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"})
	sender := NewFCMSender("proj", ts, st,
		WithBaseURL(srv.URL),
		WithBackoffs([]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}),
	)

	ctx := context.Background()
	err := sender.Send(ctx, devID, Message{Type: TypeCall, CallID: "call-1"})
	if err == nil {
		t.Fatalf("expected error after retries exhausted")
	}

	if count := atomic.LoadInt32(&attempts); count != 4 {
		t.Fatalf("expected 4 attempts, got %d", count)
	}

	pt, err := st.GetPushToken(ctx, devID)
	if err != nil || pt.FailCount != 1 {
		t.Fatalf("expected FailCount == 1 after exhausted retries, got %+v, err=%v", pt, err)
	}
}

func TestFakeSender(t *testing.T) {
	fake := NewFakeSender()
	ctx := context.Background()

	msg := Message{Type: TypeCall, CallID: "fake-1"}
	if err := fake.Send(ctx, "dev-1", msg); err != nil {
		t.Fatalf("fake send: %v", err)
	}

	sent := fake.Sent()
	if len(sent) != 1 || sent[0].DeviceID != "dev-1" || sent[0].Message.CallID != "fake-1" {
		t.Fatalf("unexpected sent: %+v", sent)
	}

	fake.Reset()
	if len(fake.Sent()) != 0 {
		t.Fatalf("expected empty sent after reset")
	}
}
