package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWMOCodeToText(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{0, "Clear sky"},
		{1, "Mainly clear"},
		{2, "Partly cloudy"},
		{3, "Overcast"},
		{45, "Fog"},
		{51, "Light drizzle"},
		{61, "Slight rain"},
		{65, "Heavy rain"},
		{71, "Slight snow"},
		{80, "Slight rain showers"},
		{95, "Thunderstorm"},
		{999, "Unknown conditions"},
	}

	for _, tt := range tests {
		got := WMOCodeToText(tt.code)
		if got != tt.want {
			t.Errorf("WMOCodeToText(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestWeatherServiceWithStubServer(t *testing.T) {
	// Stub geocoding and forecast server
	geocodeCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/search") {
			geocodeCount++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{
				"results": [{
					"name": "London",
					"latitude": 51.5085,
					"longitude": -0.1257,
					"country": "United Kingdom",
					"admin1": "England",
					"timezone": "Europe/London"
				}]
			}`)
			return
		}

		if strings.Contains(r.URL.Path, "/forecast") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{
				"timezone": "Europe/London",
				"current": {
					"temperature_2m": 18.5,
					"relative_humidity_2m": 75,
					"apparent_temperature": 17.8,
					"precipitation": 0.0,
					"weather_code": 1,
					"wind_speed_10m": 12.0
				},
				"daily": {
					"time": ["2026-09-29"],
					"weather_code": [1],
					"temperature_2m_max": [21.0],
					"temperature_2m_min": [13.5],
					"precipitation_probability_max": [15],
					"sunrise": ["2026-09-29T06:50"],
					"sunset": ["2026-09-29T18:40"]
				}
			}`)
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()

	service := NewWeatherService(server.Client())
	service.GeocodingBase = server.URL + "/search"
	service.ForecastBase = server.URL + "/forecast"

	ctx := context.Background()

	// First call should perform geocoding
	res, voiceText, err := service.GetWeather(ctx, "London", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Location != "London" || res.Country != "United Kingdom" {
		t.Errorf("expected London, United Kingdom; got %s, %s", res.Location, res.Country)
	}
	if res.Condition != "Mainly clear" {
		t.Errorf("expected 'Mainly clear', got %q", res.Condition)
	}
	if !strings.Contains(voiceText, "Weather in London, United Kingdom: 18.5°C") {
		t.Errorf("voiceText missing expected summary: %s", voiceText)
	}
	if geocodeCount != 1 {
		t.Errorf("expected 1 geocode call, got %d", geocodeCount)
	}

	// Second call should use cache for geocoding
	_, _, err = service.GetWeather(ctx, "london", 1)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if geocodeCount != 1 {
		t.Errorf("expected geocode cache hit (count=1), got %d", geocodeCount)
	}

	// Test MCP handler
	handler := service.Handler()
	toolRes, _, err := handler(ctx, nil, &WeatherParams{Location: "London"})
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if toolRes.IsError {
		t.Errorf("handler returned tool error: %+v", toolRes)
	}
	if len(toolRes.Content) == 0 {
		t.Fatalf("handler returned empty content")
	}

	// Test empty location error handling (should return IsError: true without panicking)
	errRes, _, _ := handler(ctx, nil, &WeatherParams{Location: ""})
	if !errRes.IsError {
		t.Errorf("expected IsError=true for empty location, got false")
	}
}
