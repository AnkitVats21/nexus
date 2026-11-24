package tools

import (
	"reflect"
	"testing"
)

func TestLightChanges(t *testing.T) {
	white := map[string]any{"20": true, "21": "white", "22": 500.0, "23": 0.0}
	colour := map[string]any{"20": true, "21": "colour", "24": "00f003e801f4"}
	off := map[string]any{"20": false, "21": "white"}
	tests := []struct {
		name string
		p    LightParams
		dps  map[string]any
		want map[string]any
	}{
		{"status", LightParams{}, white, map[string]any{}},
		{"off", LightParams{Power: "off", Brightness: 50}, white, map[string]any{"20": false}},
		{"on", LightParams{Power: "on"}, off, map[string]any{"20": true}},
		{"toggle on", LightParams{Power: "toggle"}, off, map[string]any{"20": true}},
		{"toggle off", LightParams{Power: "Toggle"}, white, map[string]any{"20": false}},
		{"brightness white", LightParams{Brightness: 100}, white, map[string]any{"20": true, "21": "white", "22": 1000}},
		{"brightness colour keeps hue", LightParams{Brightness: 1}, colour, map[string]any{"20": true, "24": "00f003e8000a"}},
		{"warm", LightParams{Temperature: 1, Brightness: 50}, off, map[string]any{"20": true, "21": "white", "23": 0, "22": 500}},
		{"cool", LightParams{Temperature: 100}, white, map[string]any{"20": true, "21": "white", "23": 1000}},
		{"red", LightParams{Color: "Red"}, white, map[string]any{"20": true, "21": "colour", "24": "000003e803e8"}},
		{"hex keeps colour value", LightParams{Color: "#0000ff"}, colour, map[string]any{"20": true, "21": "colour", "24": "00f003e801f4"}},
		{"white colour", LightParams{Color: "white"}, colour, map[string]any{"20": true, "21": "white", "23": 494}},
	}
	for _, tc := range tests {
		got, err := changes(tc.p, tc.dps)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, p := range []LightParams{{Power: "dim"}, {Brightness: 101}, {Temperature: -1}, {Color: "plaid"}} {
		if _, err := changes(p, white); err == nil {
			t.Errorf("%+v: no error", p)
		}
	}
}

func TestLightDescribe(t *testing.T) {
	s := NewLightService(LightConfig{Name: "work room light"})
	tests := []struct {
		dps  map[string]any
		want string
	}{
		{map[string]any{"20": false}, "The work room light is off."},
		{map[string]any{"20": true, "21": "white", "22": 1000.0, "23": 0.0}, "The work room light is on, warm white at 100 percent."},
		{map[string]any{"20": true, "21": "white", "22": 10.0, "23": 1000.0}, "The work room light is on, cool white at 1 percent."},
		{map[string]any{"20": true, "21": "colour", "24": "00f003e803e8"}, "The work room light is on, blue at 100 percent."},
		{map[string]any{"20": true, "21": "scene"}, "The work room light is on, in scene mode."},
	}
	for _, tc := range tests {
		if got := s.describe(stateOf(tc.dps)); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.dps, got, tc.want)
		}
	}
}

func TestLightToolName(t *testing.T) {
	if got := LightToolName(" Work  room Light "); got != "work_room_light" {
		t.Errorf("got %q", got)
	}
	reg := NewDefaultRegistry(Config{Lights: []LightConfig{{Name: "work room light", ID: "x", IP: "127.0.0.1", Key: "0123456789abcdef"}}})
	tool, ok := reg.Get("work_room_light")
	if !ok || tool.DeviceOnly {
		t.Fatalf("work_room_light: %v %+v", ok, tool)
	}
	if _, ok := NewDefaultRegistry(Config{}).Get("work_room_light"); ok {
		t.Error("light tool with no light configured")
	}
}
