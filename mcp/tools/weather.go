package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WMOCodeToText converts a WMO weather interpretation code into a descriptive English phrase.
func WMOCodeToText(code int) string {
	switch code {
	case 0:
		return "Clear sky"
	case 1:
		return "Mainly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45:
		return "Fog"
	case 48:
		return "Depositing rime fog"
	case 51:
		return "Light drizzle"
	case 53:
		return "Moderate drizzle"
	case 55:
		return "Dense drizzle"
	case 56:
		return "Light freezing drizzle"
	case 57:
		return "Dense freezing drizzle"
	case 61:
		return "Slight rain"
	case 63:
		return "Moderate rain"
	case 65:
		return "Heavy rain"
	case 66:
		return "Light freezing rain"
	case 67:
		return "Heavy freezing rain"
	case 71:
		return "Slight snow"
	case 73:
		return "Moderate snow"
	case 75:
		return "Heavy snow"
	case 77:
		return "Snow grains"
	case 80:
		return "Slight rain showers"
	case 81:
		return "Moderate rain showers"
	case 82:
		return "Violent rain showers"
	case 85:
		return "Slight snow showers"
	case 86:
		return "Heavy snow showers"
	case 95:
		return "Thunderstorm"
	case 96:
		return "Thunderstorm with slight hail"
	case 99:
		return "Thunderstorm with heavy hail"
	default:
		return "Unknown conditions"
	}
}

// WeatherParams defines the parameters for the weather tool.
type WeatherParams struct {
	Location string `json:"location" jsonschema:"Location name, e.g. 'London', 'Tokyo', 'Bengaluru'"`
	Days     int    `json:"days,omitempty" jsonschema:"Forecast days (1-7, default 1)"`
}

// GeoLocation stores cached geocoding data.
type GeoLocation struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Country   string  `json:"country"`
	Admin1    string  `json:"admin1,omitempty"`
	Timezone  string  `json:"timezone,omitempty"`
}

// WeatherService handles geocoding and forecast queries.
type WeatherService struct {
	HTTPClient    *http.Client
	GeocodingBase string
	ForecastBase  string
	geoCache      map[string]GeoLocation
	geoCacheMu    sync.RWMutex
}

// NewWeatherService returns a new WeatherService.
func NewWeatherService(client *http.Client) *WeatherService {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	return &WeatherService{
		HTTPClient:    client,
		GeocodingBase: "https://geocoding-api.open-meteo.com/v1/search",
		ForecastBase:  "https://api.open-meteo.com/v1/forecast",
		geoCache:      make(map[string]GeoLocation),
	}
}

type geocodingAPIResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Country   string  `json:"country"`
		Admin1    string  `json:"admin1"`
		Timezone  string  `json:"timezone"`
	} `json:"results"`
}

type forecastAPIResponse struct {
	Timezone string `json:"timezone"`
	Current  struct {
		Time          string  `json:"time"`
		Temperature   float64 `json:"temperature_2m"`
		Humidity      int     `json:"relative_humidity_2m"`
		ApparentTemp  float64 `json:"apparent_temperature"`
		Precipitation float64 `json:"precipitation"`
		WeatherCode   int     `json:"weather_code"`
		WindSpeed     float64 `json:"wind_speed_10m"`
	} `json:"current"`
	Daily struct {
		Time          []string  `json:"time"`
		WeatherCode   []int     `json:"weather_code"`
		TempMax       []float64 `json:"temperature_2m_max"`
		TempMin       []float64 `json:"temperature_2m_min"`
		PrecipProbMax []int     `json:"precipitation_probability_max"`
		Sunrise       []string  `json:"sunrise"`
		Sunset        []string  `json:"sunset"`
	} `json:"daily"`
}

// Geocode resolves a location string to coordinates, caching results in memory.
func (s *WeatherService) Geocode(ctx context.Context, location string) (GeoLocation, error) {
	key := strings.ToLower(strings.TrimSpace(location))
	if key == "" {
		return GeoLocation{}, fmt.Errorf("location cannot be empty")
	}

	s.geoCacheMu.RLock()
	cached, ok := s.geoCache[key]
	s.geoCacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	endpoint := fmt.Sprintf("%s?name=%s&count=1&language=en&format=json", s.GeocodingBase, url.QueryEscape(location))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return GeoLocation{}, fmt.Errorf("failed to create geocoding request: %w", err)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return GeoLocation{}, fmt.Errorf("geocoding request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return GeoLocation{}, fmt.Errorf("geocoding service returned status %d", resp.StatusCode)
	}

	var geoResp geocodingAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&geoResp); err != nil {
		return GeoLocation{}, fmt.Errorf("failed to decode geocoding response: %w", err)
	}

	if len(geoResp.Results) == 0 {
		return GeoLocation{}, fmt.Errorf("location not found: %s", location)
	}

	res := geoResp.Results[0]
	loc := GeoLocation{
		Name:      res.Name,
		Latitude:  res.Latitude,
		Longitude: res.Longitude,
		Country:   res.Country,
		Admin1:    res.Admin1,
		Timezone:  res.Timezone,
	}

	s.geoCacheMu.Lock()
	s.geoCache[key] = loc
	s.geoCacheMu.Unlock()

	return loc, nil
}

// ForecastResult holds structured weather information.
type ForecastResult struct {
	Location      string      `json:"location"`
	Country       string      `json:"country,omitempty"`
	CurrentTemp   float64     `json:"current_temp_c"`
	FeelsLike     float64     `json:"feels_like_c"`
	Condition     string      `json:"condition"`
	Humidity      int         `json:"humidity_pct"`
	WindSpeedKmH  float64     `json:"wind_speed_kmh"`
	Precipitation float64     `json:"precipitation_mm"`
	TodayHigh     float64     `json:"today_high_c"`
	TodayLow      float64     `json:"today_low_c"`
	RainChancePct int         `json:"rain_chance_pct"`
	Sunrise       string      `json:"sunrise,omitempty"`
	Sunset        string      `json:"sunset,omitempty"`
	DailyForecast []DailyItem `json:"daily_forecast,omitempty"`
}

// DailyItem holds daily forecast summary.
type DailyItem struct {
	Date          string  `json:"date"`
	Condition     string  `json:"condition"`
	HighC         float64 `json:"high_c"`
	LowC          float64 `json:"low_c"`
	RainChancePct int     `json:"rain_chance_pct"`
	Sunrise       string  `json:"sunrise,omitempty"`
	Sunset        string  `json:"sunset,omitempty"`
}

// GetWeather fetches current and forecast weather for a location.
func (s *WeatherService) GetWeather(ctx context.Context, location string, days int) (*ForecastResult, string, error) {
	if days < 1 {
		days = 1
	}
	if days > 7 {
		days = 7
	}

	geo, err := s.Geocode(ctx, location)
	if err != nil {
		return nil, "", err
	}

	endpoint := fmt.Sprintf(
		"%s?latitude=%.5f&longitude=%.5f&current=temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m&daily=weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,sunrise,sunset&timezone=auto&forecast_days=%d",
		s.ForecastBase, geo.Latitude, geo.Longitude, days,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create forecast request: %w", err)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("forecast request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("forecast service returned status %d", resp.StatusCode)
	}

	var fResp forecastAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&fResp); err != nil {
		return nil, "", fmt.Errorf("failed to decode forecast response: %w", err)
	}

	condText := WMOCodeToText(fResp.Current.WeatherCode)
	result := &ForecastResult{
		Location:      geo.Name,
		Country:       geo.Country,
		CurrentTemp:   fResp.Current.Temperature,
		FeelsLike:     fResp.Current.ApparentTemp,
		Condition:     condText,
		Humidity:      fResp.Current.Humidity,
		WindSpeedKmH:  fResp.Current.WindSpeed,
		Precipitation: fResp.Current.Precipitation,
	}

	// Sunrise / Sunset helper: strip date if prefixed (e.g. 2026-09-28T06:56 -> 06:56)
	cleanTime := func(t string) string {
		parts := strings.Split(t, "T")
		if len(parts) == 2 {
			return parts[1]
		}
		return t
	}

	if len(fResp.Daily.TempMax) > 0 {
		result.TodayHigh = fResp.Daily.TempMax[0]
		result.TodayLow = fResp.Daily.TempMin[0]
		if len(fResp.Daily.PrecipProbMax) > 0 {
			result.RainChancePct = fResp.Daily.PrecipProbMax[0]
		}
		if len(fResp.Daily.Sunrise) > 0 {
			result.Sunrise = cleanTime(fResp.Daily.Sunrise[0])
		}
		if len(fResp.Daily.Sunset) > 0 {
			result.Sunset = cleanTime(fResp.Daily.Sunset[0])
		}
	}

	for i := 0; i < len(fResp.Daily.Time); i++ {
		dailyCond := WMOCodeToText(fResp.Daily.WeatherCode[i])
		item := DailyItem{
			Date:      fResp.Daily.Time[i],
			Condition: dailyCond,
			HighC:     fResp.Daily.TempMax[i],
			LowC:      fResp.Daily.TempMin[i],
		}
		if i < len(fResp.Daily.PrecipProbMax) {
			item.RainChancePct = fResp.Daily.PrecipProbMax[i]
		}
		if i < len(fResp.Daily.Sunrise) {
			item.Sunrise = cleanTime(fResp.Daily.Sunrise[i])
		}
		if i < len(fResp.Daily.Sunset) {
			item.Sunset = cleanTime(fResp.Daily.Sunset[i])
		}
		result.DailyForecast = append(result.DailyForecast, item)
	}

	// Concise voice-friendly summary
	var sb strings.Builder
	locLabel := geo.Name
	if geo.Country != "" {
		locLabel = fmt.Sprintf("%s, %s", geo.Name, geo.Country)
	}

	sb.WriteString(fmt.Sprintf(
		"Weather in %s: %.1f°C (feels like %.1f°C), %s. Wind %.0f km/h, humidity %d%%.\nToday's forecast: High %.1f°C, Low %.1f°C, %d%% rain chance.",
		locLabel, result.CurrentTemp, result.FeelsLike, strings.ToLower(result.Condition),
		result.WindSpeedKmH, result.Humidity, result.TodayHigh, result.TodayLow, result.RainChancePct,
	))

	if result.Sunrise != "" && result.Sunset != "" {
		sb.WriteString(fmt.Sprintf(" Sunrise at %s, sunset at %s.", result.Sunrise, result.Sunset))
	}

	if days > 1 && len(result.DailyForecast) > 1 {
		sb.WriteString("\nUpcoming days:")
		for i := 1; i < len(result.DailyForecast); i++ {
			d := result.DailyForecast[i]
			sb.WriteString(fmt.Sprintf("\n• %s: High %.1f°C / Low %.1f°C, %s (%d%% rain)", d.Date, d.HighC, d.LowC, d.Condition, d.RainChancePct))
		}
	}

	return result, sb.String(), nil
}

// Handler returns an MCP tool handler for the weather tool.
func (s *WeatherService) Handler() func(ctx context.Context, req *mcp.CallToolRequest, params *WeatherParams) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, params *WeatherParams) (*mcp.CallToolResult, any, error) {
		if params == nil || strings.TrimSpace(params.Location) == "" {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "Location parameter is required"}},
			}, nil, nil
		}

		res, voiceText, err := s.GetWeather(ctx, params.Location, params.Days)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Weather lookup error: %s", err.Error())}},
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: voiceText}},
		}, res, nil
	}
}
