// Package bluetooth drives BlueZ through bluetoothctl for Orbit on a Pi:
// which speaker is connected (the presence gate), and scan / pair /
// connect / forget for the dashboard's Bluetooth card (HTTP API in api.go).
//
// bluetoothctl rather than D-Bus keeps it small; every call is a short
// non-interactive run, so nothing is left registered when Orbit stops.
package bluetooth

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Device is one known (paired or recently discovered) device.
type Device struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	Paired    bool   `json:"paired"`
	Trusted   bool   `json:"trusted"`
	Connected bool   `json:"connected"`
	Audio     bool   `json:"audio"` // an A2DP sink: a speaker or headphones
	Icon      string `json:"icon,omitempty"`
	RSSI      int    `json:"rssi,omitempty"`
}

// Adapter is the controller's state.
type Adapter struct {
	MAC         string `json:"mac"`
	Powered     bool   `json:"powered"`
	Discovering bool   `json:"discovering"`
}

// Ctl runs bluetoothctl.
type Ctl struct {
	Path string // default "bluetoothctl"
	// OneSpeaker: Watch keeps at most one audio sink connected and
	// disconnects the others, so the speaker Orbit is named after is the
	// one the music plays on.
	OneSpeaker bool

	mu       sync.Mutex
	prefer   string // the speaker last chosen (Prefer), wins over the others
	preferAt time.Time
}

// preferFor is how long a chosen speaker wins while it is not connected yet
// (pairing and connecting take seconds); once connected it wins until it
// goes.
const preferFor = 2 * time.Minute

// Prefer marks mac as the speaker the user chose (connect or pair): with
// OneSpeaker, Watch keeps it and lets the others go.
func (c *Ctl) Prefer(mac string) {
	c.mu.Lock()
	c.prefer, c.preferAt = mac, time.Now()
	c.mu.Unlock()
}

var (
	ansi  = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x01|\x02`)
	macRe = regexp.MustCompile(`^[0-9A-F]{2}(:[0-9A-F]{2}){5}$`)
)

// ValidMAC reports whether s is a MAC address as BlueZ prints it.
func ValidMAC(s string) bool { return macRe.MatchString(s) }

func (c *Ctl) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	path := c.Path
	if path == "" {
		path = "bluetoothctl"
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	s := ansi.ReplaceAllString(string(out), "")
	if err != nil && ctx.Err() == nil {
		return s, fmt.Errorf("bluetoothctl %s: %v: %s", args[0], err, strings.TrimSpace(s))
	}
	return s, nil
}

// Adapter reads the controller's state.
func (c *Ctl) Adapter(ctx context.Context) (Adapter, error) {
	out, err := c.run(ctx, 5*time.Second, "show")
	if err != nil {
		return Adapter{}, err
	}
	return parseAdapter(out), nil
}

func parseAdapter(out string) Adapter {
	var a Adapter
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Controller ") {
			if f := strings.Fields(line); len(f) > 1 {
				a.MAC = f[1]
			}
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch k {
		case "Powered":
			a.Powered = v == "yes"
		case "Discovering":
			a.Discovering = v == "yes"
		}
	}
	return a
}

// Devices lists the devices BlueZ knows, with their state. filter is "",
// "Paired" or "Connected".
func (c *Ctl) Devices(ctx context.Context, filter string) ([]Device, error) {
	args := []string{"devices"}
	if filter != "" {
		args = append(args, filter)
	}
	out, err := c.run(ctx, 5*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var devs []Device
	for _, d := range parseDeviceList(out) {
		info, err := c.run(ctx, 5*time.Second, "info", d.MAC)
		if err != nil {
			continue // gone meanwhile
		}
		devs = append(devs, parseInfo(d.MAC, info))
	}
	return devs, nil
}

// parseDeviceList reads "Device <mac> <name>" lines.
func parseDeviceList(out string) []Device {
	var devs []Device
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.SplitN(strings.TrimSpace(sc.Text()), " ", 3)
		if len(f) < 2 || f[0] != "Device" || !ValidMAC(f[1]) {
			continue
		}
		d := Device{MAC: f[1]}
		if len(f) == 3 {
			d.Name = f[2]
		}
		devs = append(devs, d)
	}
	return devs
}

// parseInfo reads `bluetoothctl info <mac>`.
func parseInfo(mac, out string) Device {
	d := Device{MAC: mac}
	name := ""
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok {
			continue
		}
		switch k {
		case "Name":
			name = v
		case "Alias":
			d.Name = v
		case "Paired":
			d.Paired = v == "yes"
		case "Trusted":
			d.Trusted = v == "yes"
		case "Connected":
			d.Connected = v == "yes"
		case "Icon":
			d.Icon = v
			if strings.HasPrefix(v, "audio-") {
				d.Audio = true
			}
		case "Class":
			// "0x00240404 (2360324)": a discovered device may list no
			// UUIDs before pairing, but its class says what it is.
			var cod uint32
			if _, err := fmt.Sscanf(v, "0x%x", &cod); err == nil && audioClass(cod) {
				d.Audio = true
			}
		case "UUID":
			if strings.HasPrefix(v, "Audio Sink") || strings.Contains(v, "0000110b-") {
				d.Audio = true
			}
		case "RSSI":
			// "0xffffffc4 (-60)" or "-60"
			if i := strings.IndexByte(v, '('); i >= 0 {
				v = strings.TrimSuffix(v[i+1:], ")")
			}
			fmt.Sscanf(v, "%d", &d.RSSI)
		}
	}
	if d.Name == "" {
		d.Name = name
	}
	return d
}

// audioClass reports whether a Class of Device is something that plays
// audio: major class Audio/Video with a speaker-like minor class (headset,
// hands-free, loudspeaker, headphones, portable, car, HiFi). Microphones,
// cameras and displays share the major class and are left out.
func audioClass(cod uint32) bool {
	if (cod>>8)&0x1f != 0x04 {
		return false
	}
	switch (cod >> 2) & 0x3f {
	case 0x01, 0x02, 0x05, 0x06, 0x07, 0x08, 0x0a:
		return true
	}
	return false
}

// ConnectedSink is the first connected audio sink, if any.
func (c *Ctl) ConnectedSink(ctx context.Context) (Device, bool, error) {
	sinks, err := c.connectedSinks(ctx)
	if err != nil || len(sinks) == 0 {
		return Device{}, false, err
	}
	return sinks[0], true, nil
}

func (c *Ctl) connectedSinks(ctx context.Context) ([]Device, error) {
	devs, err := c.Devices(ctx, "Connected")
	if err != nil {
		return nil, err
	}
	var sinks []Device
	for _, d := range devs {
		if d.Connected && d.Audio {
			sinks = append(sinks, d)
		}
	}
	return sinks, nil
}

// pick chooses the speaker to keep among the connected sinks: the one the
// user chose (Prefer), else the one in use (cur), else the first. A speaker
// that reconnects by itself (a trusted one, switched on) does not take over.
func (c *Ctl) pick(sinks []Device, cur Device, have bool) (Device, bool) {
	if len(sinks) == 0 {
		return Device{}, false
	}
	c.mu.Lock()
	prefer := c.prefer
	if prefer != "" && !hasMAC(sinks, prefer) && time.Since(c.preferAt) > preferFor {
		c.prefer, prefer = "", ""
	}
	c.mu.Unlock()
	for _, mac := range []string{prefer, cur.MAC} {
		if mac == "" || (mac == cur.MAC && !have) {
			continue
		}
		for _, d := range sinks {
			if d.MAC == mac {
				return d, true
			}
		}
	}
	return sinks[0], true
}

func hasMAC(devs []Device, mac string) bool {
	for _, d := range devs {
		if d.MAC == mac {
			return true
		}
	}
	return false
}

// Scan discovers devices for d (bluetoothctl keeps discovery on while it
// runs). Blocks until done. BR/EDR only: speakers play A2DP over classic
// Bluetooth, and most of what an LE scan finds (phones, watches, tags) is
// not one.
func (c *Ctl) Scan(ctx context.Context, d time.Duration) error {
	secs := fmt.Sprint(int(d.Seconds()))
	_, err := c.run(ctx, d+10*time.Second, "--timeout", secs, "scan", "bredr")
	return err
}

// Pair pairs, trusts (so it may reconnect by itself) and connects a
// discovered device. Speakers pair without a PIN (NoInputNoOutput).
func (c *Ctl) Pair(ctx context.Context, mac string) error {
	if _, err := c.run(ctx, 5*time.Second, "pairable", "on"); err != nil {
		return err
	}
	out, err := c.run(ctx, 40*time.Second, "--agent", "NoInputNoOutput", "--timeout", "30", "pair", mac)
	if err != nil {
		return err
	}
	if info, _ := c.run(ctx, 5*time.Second, "info", mac); !parseInfo(mac, info).Paired {
		return fmt.Errorf("pairing failed: %s", lastLine(out))
	}
	if _, err := c.run(ctx, 10*time.Second, "trust", mac); err != nil {
		return err
	}
	return c.Connect(ctx, mac)
}

// Connect connects a paired device.
func (c *Ctl) Connect(ctx context.Context, mac string) error {
	out, err := c.run(ctx, 30*time.Second, "--timeout", "20", "connect", mac)
	if err != nil {
		return err
	}
	if info, _ := c.run(ctx, 5*time.Second, "info", mac); !parseInfo(mac, info).Connected {
		return fmt.Errorf("could not connect: %s", lastLine(out))
	}
	return nil
}

func (c *Ctl) Disconnect(ctx context.Context, mac string) error {
	_, err := c.run(ctx, 15*time.Second, "disconnect", mac)
	return err
}

// Remove forgets a device (unpairs it).
func (c *Ctl) Remove(ctx context.Context, mac string) error {
	_, err := c.run(ctx, 15*time.Second, "remove", mac)
	return err
}

func (c *Ctl) Power(ctx context.Context, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	_, err := c.run(ctx, 10*time.Second, "power", v)
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ── The presence gate ───────────────────────────────────────────────────────

// Watch polls for a connected audio sink every interval and calls fn when
// it changes: (device, true) when one connects (or a different one takes
// over), (Device{}, false) when it goes. The first poll always reports; a
// send on poke polls at once. With OneSpeaker it disconnects every sink but
// the one it reports (pick). Returns when ctx ends.
func (c *Ctl) Watch(ctx context.Context, interval time.Duration, poke <-chan struct{}, fn func(Device, bool)) {
	var cur Device
	var have, first = false, true
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sinks, err := c.connectedSinks(ctx)
		if err == nil {
			d, ok := c.pick(sinks, cur, have)
			if c.OneSpeaker {
				for _, o := range sinks {
					if o.MAC != d.MAC {
						slog.Info("One speaker at a time: disconnecting", "speaker", o.Name, "keeping", d.Name)
						if err := c.Disconnect(ctx, o.MAC); err != nil {
							slog.Warn("Could not disconnect a second speaker", "speaker", o.Name, "err", err)
						}
					}
				}
			}
			if first || ok != have || (ok && d.MAC != cur.MAC) {
				first = false
				cur, have = d, ok
				fn(d, ok)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-poke:
		}
	}
}
