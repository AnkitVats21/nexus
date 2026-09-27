package audio

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ProcessManager manages the lifecycle of a headless mpv daemon.
type ProcessManager struct {
	mpvPath    string
	socketPath string
	cmd        *exec.Cmd
	ownsProc   bool
	log        *slog.Logger
}

// NewProcessManager creates a ProcessManager.
func NewProcessManager(mpvPath, socketPath string) *ProcessManager {
	return &ProcessManager{
		mpvPath:    mpvPath,
		socketPath: socketPath,
		log:        slog.Default().With("component", "mpv_process"),
	}
}

// EnsureRunning checks if an mpv instance is already listening on the socket,
// or starts a new headless mpv instance if needed.
func (m *ProcessManager) EnsureRunning(ctx context.Context) error {
	// 1. Check if an instance is already listening on the socket
	if conn, err := net.Dial("unix", m.socketPath); err == nil {
		_ = conn.Close()
		m.log.Info("Existing mpv IPC instance detected; attaching to socket", "socket", m.socketPath)
		m.ownsProc = false
		return nil
	}

	// 2. Remove stale socket if it exists
	if _, err := os.Stat(m.socketPath); err == nil {
		m.log.Info("Cleaning up stale mpv socket", "socket", m.socketPath)
		_ = os.Remove(m.socketPath)
	}

	// Ensure parent directory exists
	dir := filepath.Dir(m.socketPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory for mpv socket: %w", err)
	}

	// 3. Spawn mpv process
	args := []string{
		"--idle=yes",
		"--no-video",
		"--vo=null",
		"--ytdl=yes",
		fmt.Sprintf("--input-ipc-server=%s", m.socketPath),
		"--really-quiet",
	}

	m.log.Info("Spawning headless mpv process", "binary", m.mpvPath, "socket", m.socketPath)
	cmd := exec.CommandContext(ctx, m.mpvPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // run in its own process group
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start mpv process: %w", err)
	}

	m.cmd = cmd
	m.ownsProc = true

	// 4. Wait for socket to become active (up to 3 seconds)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", m.socketPath); err == nil {
			_ = conn.Close()
			m.log.Info("mpv daemon started and ready")
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	// If timeout occurred, clean up process
	_ = m.Stop()
	return fmt.Errorf("timed out waiting for mpv socket %s to become ready", m.socketPath)
}

// Stop terminates the spawned mpv process gracefully if owned by this manager.
func (m *ProcessManager) Stop() error {
	if !m.ownsProc || m.cmd == nil || m.cmd.Process == nil {
		return nil
	}

	m.log.Info("Terminating spawned mpv process", "pid", m.cmd.Process.Pid)

	// Send SIGTERM to process group
	pgid, err := syscall.Getpgid(m.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = m.cmd.Process.Signal(syscall.SIGTERM)
	}

	// Wait up to 1 second for graceful exit, then force kill
	done := make(chan error, 1)
	go func() {
		done <- m.cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		m.log.Warn("mpv did not exit on SIGTERM; sending SIGKILL")
		if pgid, err := syscall.Getpgid(m.cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = m.cmd.Process.Kill()
		}
	}

	// Clean up socket file
	_ = os.Remove(m.socketPath)
	return nil
}
