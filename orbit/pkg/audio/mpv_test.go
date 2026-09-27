package audio

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMPVIPCIntegration(t *testing.T) {
	if _, err := os.Stat("/usr/bin/mpv"); err != nil {
		t.Skip("mpv binary not found, skipping integration test")
	}

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "test-mpv.sock")

	pm := NewProcessManager("/usr/bin/mpv", sockPath)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := pm.EnsureRunning(ctx); err != nil {
		t.Fatalf("failed to start mpv: %v", err)
	}
	defer pm.Stop()

	client := NewMPVIPCClient(sockPath)
	if err := client.Connect(); err != nil {
		t.Fatalf("failed to connect to mpv: %v", err)
	}
	defer client.Close()

	// Verify volume command
	if err := client.SetVolume(85); err != nil {
		t.Fatalf("failed to set volume: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	status, err := client.GetStatus()
	if err != nil {
		t.Fatalf("failed to get status: %v", err)
	}

	if status.Volume != 85 {
		t.Errorf("expected volume 85, got %d", status.Volume)
	}
}
