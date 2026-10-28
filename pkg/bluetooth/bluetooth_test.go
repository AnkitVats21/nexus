package bluetooth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Output as BlueZ 5.79's bluetoothctl prints it.
const infoSpeaker = `Device 11:22:33:44:55:66 (public)
	Name: JBL Flip 5
	Alias: JBL Flip 5
	Class: 0x00240414 (2360340)
	Icon: audio-card
	Paired: yes
	Bonded: yes
	Trusted: yes
	Blocked: no
	Connected: yes
	LegacyPairing: no
	UUID: Audio Sink                (0000110b-0000-1000-8000-00805f9b34fb)
	UUID: A/V Remote Control Target (0000110c-0000-1000-8000-00805f9b34fb)
	RSSI: 0xffffffc4 (-60)
`

const infoPhone = `Device AA:BB:CC:DD:EE:FF (public)
	Name: Phone
	Alias: Phone
	Icon: phone
	Paired: no
	Trusted: no
	Connected: no
	UUID: Audio Source              (0000110a-0000-1000-8000-00805f9b34fb)
`

func TestParseInfo(t *testing.T) {
	d := parseInfo("11:22:33:44:55:66", "\x1b[0;94m"+infoSpeaker)
	if d.Name != "JBL Flip 5" || !d.Paired || !d.Trusted || !d.Connected || !d.Audio || d.RSSI != -60 {
		t.Fatalf("%+v", d)
	}
	p := parseInfo("AA:BB:CC:DD:EE:FF", infoPhone)
	if p.Audio || p.Paired || p.Connected {
		t.Fatalf("phone %+v", p)
	}
}

func TestParseLists(t *testing.T) {
	devs := parseDeviceList("Device 11:22:33:44:55:66 JBL Flip 5\nDevice AA:BB:CC:DD:EE:FF Phone\n[CHG] noise\n")
	if len(devs) != 2 || devs[0].Name != "JBL Flip 5" {
		t.Fatalf("%+v", devs)
	}
	a := parseAdapter("Controller B8:27:EB:00:00:01 (public)\n\tPowered: yes\n\tDiscovering: no\n")
	if a.MAC != "B8:27:EB:00:00:01" || !a.Powered || a.Discovering {
		t.Fatalf("%+v", a)
	}
}

// fakeCtl writes a bluetoothctl stand-in script answering from files in dir.
func fakeCtl(t *testing.T) (*Ctl, string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
d=$(dirname "$0")
echo "$@" >> "$d/calls"
while [ "${1#--}" != "$1" ]; do shift; [ "$1" = NoInputNoOutput ] || [ -n "${1##*[!0-9]*}" ] && shift; done
case "$1" in
show) cat "$d/show" ;;
devices) if [ "$2" = Connected ]; then cat "$d/connected" 2>/dev/null || true; else cat "$d/devices"; fi ;;
info) cat "$d/info-$2" 2>/dev/null || exit 1 ;;
*) exit 0 ;;
esac
`
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("bluetoothctl", script)
	write("show", "Controller B8:27:EB:00:00:01 (public)\n\tPowered: yes\n\tDiscovering: no\n")
	write("devices", "Device 11:22:33:44:55:66 JBL Flip 5\nDevice AA:BB:CC:DD:EE:FF Phone\n")
	write("info-11:22:33:44:55:66", infoSpeaker)
	write("info-AA:BB:CC:DD:EE:FF", infoPhone)
	return &Ctl{Path: filepath.Join(dir, "bluetoothctl")}, dir
}

func TestWatch(t *testing.T) {
	ctl, dir := fakeCtl(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 10)
	go ctl.Watch(ctx, 20*time.Millisecond, nil, func(d Device, ok bool) {
		if ok {
			events <- "up " + d.Name
		} else {
			events <- "down"
		}
	})
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %q", want)
		}
	}
	expect("down") // first poll: nothing connected
	os.WriteFile(filepath.Join(dir, "connected"), []byte("Device 11:22:33:44:55:66 JBL Flip 5\n"), 0o644)
	expect("up JBL Flip 5")
	// A connected phone (not a sink) doesn't count.
	os.WriteFile(filepath.Join(dir, "connected"), []byte("Device AA:BB:CC:DD:EE:FF Phone\n"), 0o644)
	expect("down")
}

func TestAPI(t *testing.T) {
	ctl, dir := fakeCtl(t)
	api := &API{Ctl: ctl}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bluetooth")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Adapter Adapter
		Devices []Device
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if !st.Adapter.Powered || len(st.Devices) != 2 || !st.Devices[0].Audio {
		t.Fatalf("%+v", st)
	}

	post := func(path, body string) int {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/bluetooth/pair", `{"mac":"nope; rm -rf /"}`); code != http.StatusBadRequest {
		t.Fatalf("bad mac: %d", code)
	}
	if code := post("/bluetooth/disconnect", `{"mac":"11:22:33:44:55:66"}`); code != http.StatusAccepted {
		t.Fatalf("disconnect: %d", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
		if strings.Contains(string(calls), "disconnect 11:22:33:44:55:66") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls: %s", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
