package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndDeleteOrbitToken(t *testing.T) {
	tmpDir := t.TempDir()
	tokPath := filepath.Join(tmpDir, "sub", "orbit_token")

	tok := "test-secret-orbit-token-456"
	if err := SaveOrbitToken(tokPath, tok); err != nil {
		t.Fatalf("SaveOrbitToken failed: %v", err)
	}

	data, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatalf("failed to read token file: %v", err)
	}
	if strings.TrimSpace(string(data)) != tok {
		t.Fatalf("expected token %q, got %q", tok, strings.TrimSpace(string(data)))
	}

	info, err := os.Stat(tokPath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected file perm 0600, got %o", perm)
	}

	pinfo, err := os.Stat(filepath.Dir(tokPath))
	if err != nil {
		t.Fatalf("dir stat failed: %v", err)
	}
	if perm := pinfo.Mode().Perm(); perm != 0700 {
		t.Fatalf("expected dir perm 0700, got %o", perm)
	}

	// Delete
	if err := DeleteOrbitToken(tokPath); err != nil {
		t.Fatalf("DeleteOrbitToken failed: %v", err)
	}
	if _, err := os.Stat(tokPath); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, stat err: %v", err)
	}

	// Deleting again should be a no-op
	if err := DeleteOrbitToken(tokPath); err != nil {
		t.Fatalf("subsequent DeleteOrbitToken failed: %v", err)
	}
}
