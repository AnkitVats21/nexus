package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AnkitVats21/nexus-mcp/tools"
)

// Tools is what the voice session and the music lookups use to run tools:
// the remote MCP client (MCPClient), the tools run in-process (LocalTools),
// or both (ToolSet).
type Tools interface {
	// Declarations are the tools as Gemini function declarations: device-only
	// tools, names in skip and (with builtin search) web_search left out.
	Declarations(ctx context.Context, skip map[string]bool, builtinSearch bool) []map[string]any
	// Has reports whether the tool exists here.
	Has(name string) bool
	// Call runs a tool and returns the result as the board formats it for
	// Gemini: {"result": text, "structuredContent"?} or {"status":"error",...}.
	Call(ctx context.Context, name string, args json.RawMessage) json.RawMessage
	// CallStructured runs a tool and decodes its structured result into out
	// (the device-only music tools). A tool error is an error.
	CallStructured(ctx context.Context, name string, args json.RawMessage, out any) error
}

// noTools stands in when there are none.
type noTools struct{}

func (noTools) Declarations(context.Context, map[string]bool, bool) []map[string]any { return nil }
func (noTools) Has(string) bool                                                      { return false }
func (noTools) Call(_ context.Context, name string, _ json.RawMessage) json.RawMessage {
	return toolError("no such tool: " + name)
}
func (noTools) CallStructured(_ context.Context, name string, _ json.RawMessage, _ any) error {
	return errors.New("no such tool: " + name)
}

// ─────────────────────────────────────────────────────────────────────────────
// Local: nexus-mcp's tools, run in this process (the shared Go package).

// LocalTools runs the tools of a nexus-mcp registry in-process. The same code
// serves the board through the standalone server when the hub is away.
type LocalTools struct {
	reg  *tools.Registry
	log  *slog.Logger
	skip map[string]bool // not run here (no config): the remote server's
}

// NewLocalTools wraps reg, leaving out the tools named in skip.
func NewLocalTools(reg *tools.Registry, log *slog.Logger, skip ...string) *LocalTools {
	if log == nil {
		log = slog.Default()
	}
	l := &LocalTools{reg: reg, log: log.With("component", "localtools"), skip: map[string]bool{}}
	for _, n := range skip {
		l.skip[n] = true
	}
	return l
}

// Names lists the tools run here, in registry order.
func (l *LocalTools) Names() []string {
	var out []string
	for _, t := range l.reg.Tools() {
		if !l.skip[t.Name] {
			out = append(out, t.Name)
		}
	}
	return out
}

func (l *LocalTools) Has(name string) bool {
	if l == nil || l.skip[name] {
		return false
	}
	_, ok := l.reg.Get(name)
	return ok
}

func (l *LocalTools) Declarations(_ context.Context, skip map[string]bool, builtinSearch bool) []map[string]any {
	if l == nil {
		return nil
	}
	var list []mcpTool
	for _, t := range l.reg.Tools() {
		if l.skip[t.Name] {
			continue
		}
		mt := mcpTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
		if t.DeviceOnly {
			mt.Meta = map[string]any{"nexus/device_only": true}
		}
		list = append(list, mt)
	}
	return buildDeclarations(list, skip, builtinSearch, DefaultMCPMaxTools)
}

// run calls the tool; a panic in it is an error, not a dead hub.
func (l *LocalTools) run(ctx context.Context, name string, args json.RawMessage) (res tools.Result, err error) {
	t, ok := l.reg.Get(name)
	if !ok || l.skip[name] {
		return tools.Result{}, errors.New("no such tool: " + name)
	}
	defer func() {
		if r := recover(); r != nil {
			l.log.Error("Tool panicked", "name", name, "panic", fmt.Sprint(r))
			err = errors.New(name + " failed")
		}
	}()
	return t.Call(ctx, args)
}

func (l *LocalTools) Call(ctx context.Context, name string, args json.RawMessage) json.RawMessage {
	if l == nil {
		return toolError("no such tool: " + name)
	}
	res, err := l.run(ctx, name, args)
	if err != nil {
		return toolError(err.Error())
	}
	if res.IsError {
		msg := res.Text
		if msg == "" {
			msg = "Tool execution failed"
		}
		return toolError(msg)
	}
	out := map[string]any{"result": res.Text}
	if len(res.Structured) > 0 {
		out["structuredContent"] = res.Structured
	}
	data, _ := json.Marshal(out)
	return data
}

func (l *LocalTools) CallStructured(ctx context.Context, name string, args json.RawMessage, out any) error {
	if l == nil {
		return errors.New("no local tools")
	}
	res, err := l.run(ctx, name, args)
	if err != nil {
		return err
	}
	if res.IsError {
		if res.Text != "" {
			return errors.New(res.Text)
		}
		return errors.New(name + " failed")
	}
	if len(res.Structured) == 0 {
		return fmt.Errorf("%s: no structured result", name)
	}
	return json.Unmarshal(res.Structured, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// ToolSet: local first, the remote server for the rest.

// ToolSet routes a tool to Local when Local has it, else to Remote, so a
// tool only the remote server has still works. Either may be nil.
type ToolSet struct {
	Local, Remote Tools
	// MaxTools caps the declarations offered to Gemini (default DefaultMCPMaxTools).
	MaxTools int
}

func has(t Tools, name string) bool { return t != nil && t.Has(name) }

func (s ToolSet) Has(name string) bool { return has(s.Local, name) || has(s.Remote, name) }

func (s ToolSet) Declarations(ctx context.Context, skip map[string]bool, builtinSearch bool) []map[string]any {
	var out []map[string]any
	seen := map[string]bool{}
	if s.Local != nil {
		out = s.Local.Declarations(ctx, skip, builtinSearch)
		for _, d := range out {
			seen[d["name"].(string)] = true
		}
	}
	if s.Remote != nil {
		// A local tool shadows the remote one of the same name, offered
		// or not (a device-only one stays hidden).
		rskip := make(map[string]bool, len(skip)+len(seen))
		for k, v := range skip {
			rskip[k] = v
		}
		for k := range seen {
			rskip[k] = true
		}
		if l, ok := s.Local.(*LocalTools); ok && l != nil {
			for _, n := range l.Names() {
				rskip[n] = true
			}
		}
		out = append(out, s.Remote.Declarations(ctx, rskip, builtinSearch)...)
	}
	max := s.MaxTools
	if max <= 0 {
		max = DefaultMCPMaxTools
	}
	var kept []map[string]any
	total := 0
	for _, d := range out {
		if len(kept) >= max {
			break
		}
		size, _ := json.Marshal(d)
		if total+len(size) > mcpMaxDeclarationBytes {
			continue
		}
		total += len(size)
		kept = append(kept, d)
	}
	return kept
}

func (s ToolSet) pick(name string) Tools {
	if has(s.Local, name) {
		return s.Local
	}
	if s.Remote != nil {
		return s.Remote
	}
	return noTools{}
}

func (s ToolSet) Call(ctx context.Context, name string, args json.RawMessage) json.RawMessage {
	return s.pick(name).Call(ctx, name, args)
}

func (s ToolSet) CallStructured(ctx context.Context, name string, args json.RawMessage, out any) error {
	return s.pick(name).CallStructured(ctx, name, args, out)
}

// RemoteOnlyNames lists the names the remote server has that Local doesn't, for
// the startup log.
func RemoteOnlyNames(ctx context.Context, local *LocalTools, remote *MCPClient) []string {
	var out []string
	for _, n := range remote.ToolNames(ctx) {
		if !local.Has(n) {
			out = append(out, n)
		}
	}
	return out
}

// JoinNames is names, comma-separated, "-" when none (log text).
func JoinNames(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}
