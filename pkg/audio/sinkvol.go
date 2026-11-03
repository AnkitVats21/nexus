package audio

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"
)

// SinkVolume is the volume of PipeWire's default sink, through wpctl. With
// a Bluetooth speaker that is the speaker's own volume (AVRCP absolute
// volume): WirePlumber remembers it per speaker, so each one comes back at
// its own level, and the speaker's buttons change it too.
type SinkVolume struct {
	Path string // default "wpctl"
}

const defaultSink = "@DEFAULT_AUDIO_SINK@"

// Available reports whether wpctl is installed.
func (s SinkVolume) Available() bool {
	_, err := exec.LookPath(s.path())
	return err == nil
}

func (s SinkVolume) path() string {
	if s.Path == "" {
		return "wpctl"
	}
	return s.Path
}

// Get returns the volume 0-100 (0 when muted).
func (s SinkVolume) Get(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.path(), "get-volume", defaultSink).Output()
	if err != nil {
		return 0, fmt.Errorf("wpctl get-volume: %w", err)
	}
	return parseWpctlVolume(string(out))
}

// parseWpctlVolume reads "Volume: 0.40" or "Volume: 0.40 [MUTED]".
func parseWpctlVolume(out string) (int, error) {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "Volume:" {
		return 0, fmt.Errorf("wpctl: unexpected %q", strings.TrimSpace(out))
	}
	var v float64
	if _, err := fmt.Sscanf(f[1], "%f", &v); err != nil {
		return 0, fmt.Errorf("wpctl: unexpected %q", strings.TrimSpace(out))
	}
	if strings.Contains(out, "[MUTED]") {
		return 0, nil
	}
	return int(math.Round(v * 100)), nil
}

// Set sets the volume 0-100, unmuting above 0.
func (s SinkVolume) Set(ctx context.Context, vol int) error {
	vol = max(0, min(100, vol))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, s.path(), "set-volume", defaultSink, fmt.Sprintf("%.2f", float64(vol)/100)).Run(); err != nil {
		return fmt.Errorf("wpctl set-volume: %w", err)
	}
	mute := "0"
	if vol == 0 {
		mute = "1"
	}
	return exec.CommandContext(ctx, s.path(), "set-mute", defaultSink, mute).Run()
}
