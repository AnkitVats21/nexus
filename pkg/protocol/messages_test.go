package protocol

import (
	"encoding/json"
	"testing"
)

func TestMessageSerialization(t *testing.T) {
	// Test SatelliteRegisterMsg
	reg := SatelliteRegisterMsg{
		Cmd: "satellite_register",
		Satellite: SatelliteInfo{
			ID:           "test-sat",
			Name:         "Test Satellite",
			Type:         "desktop",
			Capabilities: []string{"audio_playback"},
		},
	}
	regBytes, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("failed to marshal register msg: %v", err)
	}

	var parsedReg SatelliteRegisterMsg
	if err := json.Unmarshal(regBytes, &parsedReg); err != nil {
		t.Fatalf("failed to unmarshal register msg: %v", err)
	}
	if parsedReg.Satellite.ID != "test-sat" {
		t.Errorf("expected test-sat, got %s", parsedReg.Satellite.ID)
	}

	// Test OrbitRouteMsg
	route := OrbitRouteMsg{
		Cmd:        "orbit_route",
		VideoID:    "dQw4w9WgXcQ",
		Title:      "Never Gonna Give You Up",
		Artist:     "Rick Astley",
		Duration:   212,
		PositionMs: 42000,
	}
	routeBytes, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("failed to marshal route msg: %v", err)
	}

	var parsedRoute OrbitRouteMsg
	if err := json.Unmarshal(routeBytes, &parsedRoute); err != nil {
		t.Fatalf("failed to unmarshal route msg: %v", err)
	}
	if parsedRoute.VideoID != "dQw4w9WgXcQ" || parsedRoute.PositionMs != 42000 {
		t.Errorf("mismatch in parsed route: %+v", parsedRoute)
	}
}
