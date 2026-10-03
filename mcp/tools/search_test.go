package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchServiceKeyResolution(t *testing.T) {
	// 1. Explicit key
	svc := NewSearchService(nil, "explicit-key", "")
	key, err := svc.resolveAPIKey()
	if err != nil || key != "explicit-key" {
		t.Fatalf("expected explicit-key, got %s (err: %v)", key, err)
	}

	// 2. From file
	tmpDir := t.TempDir()
	keyFile := filepath.Join(tmpDir, "test_key")
	if err := os.WriteFile(keyFile, []byte("file-secret-key\n"), 0600); err != nil {
		t.Fatal(err)
	}

	svc2 := NewSearchService(nil, "", keyFile)
	key2, err := svc2.resolveAPIKey()
	if err != nil || key2 != "file-secret-key" {
		t.Fatalf("expected file-secret-key, got %s (err: %v)", key2, err)
	}

	// 3. Missing key
	svc3 := NewSearchService(nil, "", "")
	_, err = svc3.resolveAPIKey()
	if err == nil {
		t.Fatalf("expected error when no key is configured")
	}
}

func TestSearchServiceWithStub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{
			"candidates": [{
				"content": {
					"parts": [{
						"text": "The latest score was 2-1 with goals in both halves."
					}]
				},
				"groundingMetadata": {
					"groundingChunks": [
						{
							"web": {
								"uri": "https://example.com/sports/match-123",
								"title": "Example Sports"
							}
						},
						{
							"web": {
								"uri": "https://news.example.org/results",
								"title": "News Results"
							}
						}
					]
				}
			}]
		}`)
	}))
	defer server.Close()

	svc := NewSearchService(server.Client(), "mock-key", "")
	svc.Endpoint = server.URL

	ctx := context.Background()
	res, voiceText, err := svc.Search(ctx, "latest match score")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(res.Answer, "The latest score was 2-1") {
		t.Errorf("expected answer, got %q", res.Answer)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(res.Sources))
	}
	if !strings.Contains(voiceText, "Sources: Example Sports (example.com), News Results (news.example.org)") {
		t.Errorf("voiceText missing expected sources: %s", voiceText)
	}

	// Test MCP handler
	handler := svc.Handler()
	toolRes, _, err := handler(ctx, nil, &WebSearchParams{Query: "test query"})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if toolRes.IsError {
		t.Errorf("unexpected tool error: %+v", toolRes)
	}
	if len(toolRes.Content) == 0 {
		t.Fatalf("expected non-empty content")
	}

	// Empty query returns tool error (IsError: true) without panic
	errRes, _, _ := handler(ctx, nil, &WebSearchParams{Query: ""})
	if !errRes.IsError {
		t.Errorf("expected IsError=true for empty query")
	}
}
