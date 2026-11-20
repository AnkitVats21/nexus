package audio

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ALSAVolume is a Bluetooth speaker's own volume through bluez-alsa's ALSA
// mixer (amixer -D bluealsa), for a box without PipeWire (the Pi). bluealsa
// sends it to the speaker as AVRCP absolute volume when the speaker takes
// it, so the speaker's buttons and this control move together.
type ALSAVolume struct {
	Device string // ALSA control device, default "bluealsa"
	Path   string // default "amixer"
}

// Available reports whether amixer is installed.
func (a ALSAVolume) Available() bool {
	_, err := exec.LookPath(a.path())
	return err == nil
}

func (a ALSAVolume) path() string {
	if a.Path == "" {
		return "amixer"
	}
	return a.Path
}

func (a ALSAVolume) device() string {
	if a.Device == "" {
		return "bluealsa"
	}
	return a.Device
}

func (a ALSAVolume) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, a.path(), append([]string{"-D", a.device()}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("amixer %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// control finds the speaker's playback control: bluealsa names it
// "<speaker> A2DP" ("<speaker> - A2DP" in some versions).
func (a ALSAVolume) control(ctx context.Context) (string, error) {
	out, err := a.run(ctx, "scontrols")
	if err != nil {
		return "", err
	}
	return pickALSAControl(out)
}

var scontrolRe = regexp.MustCompile(`^Simple mixer control '(.*)',(\d+)$`)

// pickALSAControl picks the A2DP control from `amixer scontrols`, else the
// only one there is.
func pickALSAControl(out string) (string, error) {
	var all []string
	for _, line := range strings.Split(out, "\n") {
		m := scontrolRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		name := m[1]
		if m[2] != "0" {
			name += "," + m[2]
		}
		if strings.HasSuffix(name, "A2DP") {
			return name, nil
		}
		all = append(all, name)
	}
	if len(all) == 1 {
		return all[0], nil
	}
	if len(all) == 0 {
		return "", errors.New("amixer: no mixer control (is a speaker connected?)")
	}
	return "", fmt.Errorf("amixer: no A2DP control among %q", all)
}

// Get returns the volume 0-100 (0 when switched off).
func (a ALSAVolume) Get(ctx context.Context) (int, error) {
	ctl, err := a.control(ctx)
	if err != nil {
		return 0, err
	}
	out, err := a.run(ctx, "sget", ctl)
	if err != nil {
		return 0, err
	}
	return parseAmixerVolume(out)
}

var amixerLevelRe = regexp.MustCompile(`\[(\d+)%\](?:.*\[(on|off)\])?`)

// parseAmixerVolume reads the first channel's "[NN%] ... [on|off]".
func parseAmixerVolume(out string) (int, error) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "Playback") {
			continue
		}
		m := amixerLevelRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[2] == "off" {
			return 0, nil
		}
		v, _ := strconv.Atoi(m[1])
		return max(0, min(100, v)), nil
	}
	return 0, fmt.Errorf("amixer: no playback level in %q", strings.TrimSpace(out))
}

// Set sets the volume 0-100, switching the control off at 0.
func (a ALSAVolume) Set(ctx context.Context, vol int) error {
	vol = max(0, min(100, vol))
	ctl, err := a.control(ctx)
	if err != nil {
		return err
	}
	sw := "on"
	if vol == 0 {
		sw = "off"
	}
	_, err = a.run(ctx, "-q", "sset", ctl, strconv.Itoa(vol)+"%", sw)
	return err
}
