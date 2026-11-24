package tools

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexus/pkg/tuya"
)

// A Tuya colour bulb (Havells Glamax TW + RGB), on the LAN, through its local
// protocol. Its data points:
const (
	dpSwitch = "20" // bool
	dpMode   = "21" // "white" | "colour" | "scene" | "music"
	dpBright = "22" // white brightness 10-1000
	dpTemp   = "23" // white temperature 0 (warm) - 1000 (cool)
	dpColour = "24" // HHHHSSSSVVVV hex: hue 0-360, saturation and value 0-1000
)

// LightConfig is one bulb. The hub reads it from a file and runs the tool; the
// MCP server has only the name and declares it (the hub's own copy of the
// tool, same name, shadows the server's and runs the call).
type LightConfig struct {
	Name string `json:"name"` // what the voice calls it, e.g. "work room light"
	ID   string `json:"id"`
	IP   string `json:"ip"`
	Key  string `json:"key"` // local key
}

// LightParams are the light tool's arguments; none = report the state.
type LightParams struct {
	Power       string `json:"power,omitempty" jsonschema:"on, off or toggle"`
	Brightness  int    `json:"brightness,omitempty" jsonschema:"Brightness in percent, 1-100"`
	Temperature int    `json:"white_temperature,omitempty" jsonschema:"White light, 1 = warmest (yellowish) to 100 = coolest (daylight); 50 = neutral"`
	Color       string `json:"color,omitempty" jsonschema:"A colour name (red, orange, yellow, green, cyan, blue, purple, pink, white) or hex like #ff8800"`
}

// LightState is the light as the tool reports it.
type LightState struct {
	On          bool   `json:"on"`
	Mode        string `json:"mode"` // white | colour | scene | music
	Brightness  int    `json:"brightness"`
	Temperature int    `json:"white_temperature,omitempty"`
	Color       string `json:"color,omitempty"` // #rrggbb
}

// LightService controls one bulb.
type LightService struct {
	cfg LightConfig
	dev tuya.Device
	mu  sync.Mutex // one connection at a time: the bulb takes few
}

// NewLightService returns the service for cfg.
func NewLightService(cfg LightConfig) *LightService {
	if cfg.Name == "" {
		cfg.Name = "light"
	}
	return &LightService{cfg: cfg, dev: tuya.Device{ID: cfg.ID, IP: cfg.IP, Key: cfg.Key, Timeout: 4 * time.Second}}
}

var colorNames = map[string]string{
	"red": "#ff0000", "orange": "#ff7f00", "yellow": "#ffd000", "green": "#00ff00",
	"cyan": "#00ffff", "blue": "#0000ff", "purple": "#8000ff", "violet": "#8000ff",
	"pink": "#ff40a0", "magenta": "#ff00ff",
}

// Local reports whether this service can reach the bulb (it has its address
// and key). Without them the tool is only declared: the MCP server lists it,
// the hub runs it.
func (s *LightService) Local() bool { return s.cfg.IP != "" && s.cfg.Key != "" }

// Handler is the MCP handler.
func (s *LightService) Handler() func(context.Context, *mcp.CallToolRequest, *LightParams) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, p *LightParams) (*mcp.CallToolResult, any, error) {
		if p == nil {
			p = &LightParams{}
		}
		if !s.Local() {
			// Declared here (the MCP server) and run by the home hub, which
			// has the bulb's address and key: a call here means no hub.
			return &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("The %s is controlled by the home hub, which is not connected right now.", s.cfg.Name)}}}, nil, nil
		}
		st, err := s.Apply(ctx, *p)
		if err != nil {
			return &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Could not reach the %s: %v", s.cfg.Name, err)}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s.describe(st)}}}, st, nil
	}
}

// Apply makes the change p asks for (none: just read) and returns the state.
func (s *LightService) Apply(ctx context.Context, p LightParams) (LightState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.dev.Dial(ctx)
	if err != nil {
		return LightState{}, err
	}
	defer c.Close()
	dps, err := c.Status()
	if err != nil {
		return LightState{}, err
	}
	set, err := changes(p, dps)
	if err != nil {
		return LightState{}, err
	}
	if len(set) > 0 {
		got, err := c.Set(set)
		if err != nil {
			return LightState{}, err
		}
		for k, v := range set {
			dps[k] = v
		}
		for k, v := range got {
			dps[k] = v
		}
	}
	return stateOf(dps), nil
}

// changes turns the arguments into the DPs to set, given the current DPs.
func changes(p LightParams, dps map[string]any) (map[string]any, error) {
	set := map[string]any{}
	power := strings.ToLower(strings.TrimSpace(p.Power))
	switch power {
	case "", "on", "off":
	case "toggle":
		on, _ := dps[dpSwitch].(bool)
		power = map[bool]string{true: "off", false: "on"}[on]
	default:
		return nil, fmt.Errorf("power must be on, off or toggle, not %q", p.Power)
	}
	if p.Brightness < 0 || p.Brightness > 100 {
		return nil, fmt.Errorf("brightness must be 1-100, not %d", p.Brightness)
	}
	if p.Temperature < 0 || p.Temperature > 100 {
		return nil, fmt.Errorf("white_temperature must be 1-100, not %d", p.Temperature)
	}

	color := strings.ToLower(strings.TrimSpace(p.Color))
	if color == "white" {
		color = ""
		if p.Temperature == 0 {
			p.Temperature = 50
		}
	}
	mode, _ := dps[dpMode].(string)
	switch {
	case color != "":
		hex := color
		if named, ok := colorNames[color]; ok {
			hex = named
		}
		h, sat, ok := hexToHS(hex)
		if !ok {
			return nil, fmt.Errorf("unknown colour %q", p.Color)
		}
		v := 1000
		if p.Brightness > 0 {
			v = pct(p.Brightness, 10)
		} else if mode == "colour" {
			if _, _, cv, ok := parseHSV(dps[dpColour]); ok && cv > 0 {
				v = cv
			}
		}
		set[dpMode] = "colour"
		set[dpColour] = fmt.Sprintf("%04x%04x%04x", h, sat, v)
	case p.Temperature > 0:
		set[dpMode] = "white"
		set[dpTemp] = (p.Temperature - 1) * 1000 / 99
		if p.Brightness > 0 {
			set[dpBright] = pct(p.Brightness, 10)
		}
	case p.Brightness > 0:
		if mode == "colour" {
			h, sat, _, _ := parseHSV(dps[dpColour])
			set[dpColour] = fmt.Sprintf("%04x%04x%04x", h, sat, pct(p.Brightness, 10))
		} else {
			set[dpMode] = "white"
			set[dpBright] = pct(p.Brightness, 10)
		}
	}
	// Any change of light means the light on, unless it was asked off.
	if power == "off" {
		set = map[string]any{dpSwitch: false}
	} else if power == "on" || len(set) > 0 {
		set[dpSwitch] = true
	}
	return set, nil
}

// pct maps 1-100 percent onto min-1000.
func pct(p, min int) int {
	return min + (p-1)*(1000-min)/99
}

func stateOf(dps map[string]any) LightState {
	st := LightState{}
	st.On, _ = dps[dpSwitch].(bool)
	st.Mode, _ = dps[dpMode].(string)
	switch st.Mode {
	case "colour":
		if h, s, v, ok := parseHSV(dps[dpColour]); ok {
			st.Brightness = toPct(v, 10)
			st.Color = hsToHex(h, s)
		}
	default:
		st.Brightness = toPct(num(dps[dpBright]), 10)
		if st.Mode == "white" {
			st.Temperature = 1 + num(dps[dpTemp])*99/1000
		}
	}
	return st
}

func toPct(v, min int) int {
	if v <= min {
		return 1
	}
	return 1 + int(math.Round(float64(v-min)*99/float64(1000-min)))
}

func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func (s *LightService) describe(st LightState) string {
	name := "The " + s.cfg.Name
	if !st.On {
		return name + " is off."
	}
	switch st.Mode {
	case "white":
		tone := "neutral white"
		if st.Temperature <= 33 {
			tone = "warm white"
		} else if st.Temperature > 66 {
			tone = "cool white"
		}
		return fmt.Sprintf("%s is on, %s at %d percent.", name, tone, st.Brightness)
	case "colour":
		return fmt.Sprintf("%s is on, %s at %d percent.", name, colorName(st.Color), st.Brightness)
	default:
		return fmt.Sprintf("%s is on, in %s mode.", name, st.Mode)
	}
}

// colorName is the nearest named colour by hue, for the readout.
func colorName(hex string) string {
	h, s, ok := hexToHS(hex)
	if !ok {
		return "a colour"
	}
	if s < 150 {
		return "white"
	}
	best, bestD := "a colour", 1000
	for _, n := range []string{"red", "orange", "yellow", "green", "cyan", "blue", "purple", "pink", "magenta"} {
		nh, _, _ := hexToHS(colorNames[n])
		d := int(math.Abs(float64(h - nh)))
		if d > 180 {
			d = 360 - d
		}
		if d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

// hexToHS gives hue 0-360 and saturation 0-1000 of #rrggbb.
func hexToHS(hex string) (int, int, bool) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, 0, false
	}
	r, g, b := float64(n>>16&0xff)/255, float64(n>>8&0xff)/255, float64(n&0xff)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	d := mx - mn
	var h float64
	switch {
	case d == 0:
	case mx == r:
		h = math.Mod((g-b)/d, 6)
	case mx == g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	s := 0.0
	if mx > 0 {
		s = d / mx
	}
	return int(math.Round(h)) % 360, int(math.Round(s * 1000)), true
}

// hsToHex is #rrggbb of hue 0-360 and saturation 0-1000 at full value.
func hsToHex(h, s int) string {
	sf := float64(s) / 1000
	c := sf
	x := c * (1 - math.Abs(math.Mod(float64(h)/60, 2)-1))
	m := 1 - c
	var r, g, b float64
	switch {
	case h < 60:
		r, g = c, x
	case h < 120:
		r, g = x, c
	case h < 180:
		g, b = c, x
	case h < 240:
		g, b = x, c
	case h < 300:
		r, b = x, c
	default:
		r, b = c, x
	}
	to := func(v float64) int { return int(math.Round((v + m) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", to(r), to(g), to(b))
}

func parseHSV(v any) (h, s, val int, ok bool) {
	str, _ := v.(string)
	if len(str) != 12 {
		return 0, 0, 0, false
	}
	hh, e1 := strconv.ParseUint(str[0:4], 16, 16)
	ss, e2 := strconv.ParseUint(str[4:8], 16, 16)
	vv, e3 := strconv.ParseUint(str[8:12], 16, 16)
	if e1 != nil || e2 != nil || e3 != nil {
		return 0, 0, 0, false
	}
	return int(hh), int(ss), int(vv), true
}
