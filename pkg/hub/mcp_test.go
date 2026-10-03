package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPNoRemoteCallDuringSessionSetup(t *testing.T) {
	ctx := context.Background()
	var reqCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{
				"tools": []map[string]any{
					{"name": "fetch_news", "description": "Fetches current news"},
					{"name": "lookup_stock", "description": "Stock lookup"},
				},
			}
		default:
			http.Error(w, "unknown method: "+req.Method, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}))
	defer srv.Close()

	client := NewMCPClient(srv.URL, "valid-token", 0, nil)

	// Before refresh, cache is empty and Declarations returns nil without network requests.
	decls := client.Declarations(ctx, nil, false)
	if len(decls) != 0 {
		t.Fatalf("expected empty declarations before refresh, got: %v", decls)
	}
	if atomic.LoadInt32(&reqCount) != 0 {
		t.Fatalf("expected 0 network requests before refresh, got: %d", atomic.LoadInt32(&reqCount))
	}

	// Background refresh populates the declarations.
	if err := client.Refresh(ctx); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	initReqs := atomic.LoadInt32(&reqCount)
	if initReqs == 0 {
		t.Fatal("expected network requests during Refresh")
	}

	// Simulate multiple session setups.
	for i := 0; i < 5; i++ {
		decls = client.Declarations(ctx, nil, false)
		if len(decls) != 2 {
			t.Fatalf("expected 2 declarations, got: %v", decls)
		}
	}

	// Verify that Declarations made ZERO network calls during session setups.
	finalReqs := atomic.LoadInt32(&reqCount)
	if finalReqs != initReqs {
		t.Fatalf("Declarations performed network I/O: requests increased from %d to %d", initReqs, finalReqs)
	}
}

func TestMCP401Backoff(t *testing.T) {
	ctx := context.Background()
	var reqCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		auth := r.Header.Get("Authorization")
		if auth != "Bearer token-good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"Unauthorized"}}`))
			return
		}
		var req struct {
			Method string `json:"method"`
			ID     *int   `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "tool1", "description": "tool 1"}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"text": "tool1 output"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "mcp_token")
	if err := os.WriteFile(tokenFile, []byte("token-bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := NewMCPClient(srv.URL, "token-bad", 0, nil)
	client.SetTokenFile(tokenFile)

	// Initial refresh fails with 401.
	err := client.Refresh(ctx)
	if err == nil {
		t.Fatal("expected 401 error, got nil")
	}
	if !client.InBackoff() {
		t.Fatal("expected client to be in backoff after 401")
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Fatalf("expected 1 request, got: %d", atomic.LoadInt32(&reqCount))
	}

	// Subsequent calls during backoff must NOT touch network.
	err = client.Refresh(ctx)
	if err == nil || err.Error() != "MCP auth backoff active" {
		t.Fatalf("expected backoff error, got: %v", err)
	}
	res := client.Call(ctx, "tool1", nil)
	var errObj struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(res, &errObj)
	if errObj.Status != "error" || errObj.Message != "MCP auth backoff active" {
		t.Fatalf("expected Call to fail with backoff error, got: %s", string(res))
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Fatalf("expected request count to remain 1 during backoff, got: %d", atomic.LoadInt32(&reqCount))
	}

	// Declarations must return nil without network I/O.
	if decls := client.Declarations(ctx, nil, false); len(decls) != 0 {
		t.Fatalf("expected 0 declarations in backoff, got: %v", decls)
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Fatalf("expected request count to remain 1, got: %d", atomic.LoadInt32(&reqCount))
	}

	// Update token file with valid credentials.
	// Ensure file modification timestamp changes.
	time.Sleep(15 * time.Millisecond)
	if err := os.WriteFile(tokenFile, []byte("token-good\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Updating token file should immediately clear backoff and allow successful refresh.
	if err := client.Refresh(ctx); err != nil {
		t.Fatalf("expected refresh to succeed after token update, got: %v", err)
	}
	if client.InBackoff() {
		t.Fatal("expected client backoff to be cleared")
	}
	if !client.Has("tool1") {
		t.Fatal("expected client to have tool1")
	}

	// Call now succeeds.
	callRes := client.Call(ctx, "tool1", nil)
	var callObj map[string]any
	if err := json.Unmarshal(callRes, &callObj); err != nil || callObj["result"] != "tool1 output" {
		t.Fatalf("unexpected call result: %s", string(callRes))
	}
}

func TestMCPEmptyTokenNoRemote(t *testing.T) {
	ctx := context.Background()
	var reqCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{}}`)
	}))
	defer srv.Close()

	// 1. Empty token string passed directly.
	client := NewMCPClient(srv.URL, "", 0, nil)

	err := client.Refresh(ctx)
	if err == nil || err.Error() != "remote MCP disabled: no token" {
		t.Fatalf("expected empty token error, got: %v", err)
	}
	if decls := client.Declarations(ctx, nil, false); len(decls) != 0 {
		t.Fatalf("expected empty declarations, got: %v", decls)
	}
	res := client.Call(ctx, "tool1", nil)
	if fmt.Sprint(string(res)) != `{"message":"remote MCP disabled: no token","status":"error"}` {
		t.Fatalf("unexpected call response: %s", string(res))
	}
	if atomic.LoadInt32(&reqCount) != 0 {
		t.Fatalf("expected 0 remote calls for empty token, got: %d", atomic.LoadInt32(&reqCount))
	}

	// 2. Token file that exists but is empty.
	emptyFile := filepath.Join(t.TempDir(), "empty_token")
	if err := os.WriteFile(emptyFile, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.SetTokenFile(emptyFile)

	err = client.Refresh(ctx)
	if err == nil || err.Error() != "remote MCP disabled: no token" {
		t.Fatalf("expected empty token error from file, got: %v", err)
	}
	if decls := client.Declarations(ctx, nil, false); len(decls) != 0 {
		t.Fatalf("expected empty declarations, got: %v", decls)
	}
	if atomic.LoadInt32(&reqCount) != 0 {
		t.Fatalf("expected 0 remote calls for empty token file, got: %d", atomic.LoadInt32(&reqCount))
	}
}

func TestMCPPeriodicRefreshAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{"protocolVersion":"2024-11-05","capabilities":{}}}`)
	}))
	defer srv.Close()

	client := NewMCPClient(srv.URL, "some-token", 0, nil)
	client.Start(ctx)
	// Safe to call Start multiple times.
	client.Start(ctx)
	// Safe to call Close.
	client.Close()
	client.Close()
}
