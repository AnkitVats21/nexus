package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MCP limits, the same as the board's (main/services/mcp/McpUtil.cpp).
const (
	DefaultMCPMaxTools     = 32
	mcpMaxDeclarationBytes = 24 << 10
	mcpToolsMaxAge         = 10 * time.Minute
	mcpCallTimeout         = 45 * time.Second
	mcpProtocolVersion     = "2024-11-05"
)

var mcpToolName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

// MCPClient calls an MCP server over streamable HTTP (JSON-RPC; the answer
// is JSON or server-sent events). The hub uses it for the voice session's
// remote tools, as the board does in direct mode.
type MCPClient struct {
	url      string
	token    string
	maxTools int
	http     *http.Client
	log      *slog.Logger

	mu        sync.Mutex
	session   string // Mcp-Session-Id
	inited    bool
	nextID    int
	tools     []mcpTool
	toolsAt   time.Time
	lastError string
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Meta        map[string]any  `json:"_meta"`
}

func NewMCPClient(url, token string, maxTools int, log *slog.Logger) *MCPClient {
	if maxTools <= 0 {
		maxTools = DefaultMCPMaxTools
	}
	if log == nil {
		log = slog.Default()
	}
	return &MCPClient{url: url, token: token, maxTools: maxTools,
		http: &http.Client{Timeout: mcpCallTimeout}, log: log.With("component", "mcp")}
}

// Declarations are the MCP tools as Gemini function declarations: device-only
// tools, names in skip and (with builtin search) web_search left out, then
// the board's limits on count and size. The tool list is cached.
func (c *MCPClient) Declarations(ctx context.Context, skip map[string]bool, builtinSearch bool) []map[string]any {
	if c == nil {
		return nil
	}
	tools, err := c.listTools(ctx)
	if err != nil {
		c.log.Warn("MCP tools unavailable", "err", err)
		return nil
	}
	var out []map[string]any
	total := 0
	for _, t := range tools {
		if !mcpToolName.MatchString(t.Name) || skip[t.Name] {
			continue
		}
		if v, _ := t.Meta["nexus/device_only"].(bool); v {
			continue
		}
		if builtinSearch && t.Name == "web_search" {
			continue
		}
		if len(out) >= c.maxTools {
			break
		}
		decl := map[string]any{"name": t.Name, "description": t.Description}
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
			decl["parametersJsonSchema"] = t.InputSchema
		} else {
			decl["parametersJsonSchema"] = map[string]any{"type": "object"}
		}
		size, _ := json.Marshal(decl)
		if total+len(size) > mcpMaxDeclarationBytes {
			continue
		}
		total += len(size)
		out = append(out, decl)
	}
	return out
}

// Has reports whether name is one of the cached MCP tools.
func (c *MCPClient) Has(name string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Call runs a tool and returns the result as the board formats it for
// Gemini: {"result": joined text, "structuredContent"?} or {"status":"error",...}.
func (c *MCPClient) Call(ctx context.Context, name string, args json.RawMessage) json.RawMessage {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	res, err := c.rpc(ctx, "tools/call", params)
	if err != nil {
		return toolError(err.Error())
	}
	return formatToolResult(res)
}

func (c *MCPClient) listTools(ctx context.Context) ([]mcpTool, error) {
	c.mu.Lock()
	if c.tools != nil && time.Since(c.toolsAt) < mcpToolsMaxAge {
		tools := c.tools
		c.mu.Unlock()
		return tools, nil
	}
	c.mu.Unlock()
	res, err := c.rpc(ctx, "tools/list", json.RawMessage("{}"))
	if err != nil {
		c.mu.Lock()
		tools := c.tools // a stale list beats none
		c.mu.Unlock()
		if tools != nil {
			return tools, nil
		}
		return nil, err
	}
	var list struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	c.mu.Lock()
	c.tools = list.Tools
	c.toolsAt = time.Now()
	c.mu.Unlock()
	c.log.Info("MCP tools listed", "count", len(list.Tools))
	return list.Tools, nil
}

// rpc sends one request (initializing the session first, and again once if
// the server has forgotten it) and returns its "result".
func (c *MCPClient) rpc(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensureInit(ctx); err != nil {
			return nil, err
		}
		res, status, err := c.post(ctx, method, params, true)
		if status == http.StatusNotFound {
			c.mu.Lock()
			c.inited = false
			c.mu.Unlock()
			continue
		}
		return res, err
	}
	return nil, errors.New("MCP session lost")
}

func (c *MCPClient) ensureInit(ctx context.Context) error {
	c.mu.Lock()
	inited := c.inited
	if !inited {
		c.session = ""
	}
	c.mu.Unlock()
	if inited {
		return nil
	}
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "nexus-hub", "version": "1.0.0"},
	})
	if _, _, err := c.post(ctx, "initialize", params, true); err != nil {
		return fmt.Errorf("MCP initialize: %w", err)
	}
	c.post(ctx, "notifications/initialized", nil, false)
	c.mu.Lock()
	c.inited = true
	c.mu.Unlock()
	return nil
}

func (c *MCPClient) post(ctx context.Context, method string, params json.RawMessage, withID bool) (json.RawMessage, int, error) {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	session := c.session
	c.mu.Unlock()
	if withID {
		req["id"] = id
	}
	if params != nil {
		req["params"] = params
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.token)
	}
	if session != "" {
		hr.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.mu.Lock()
		c.session = s
		c.mu.Unlock()
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	if !withID {
		return nil, resp.StatusCode, nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data = sseResult(data, id)
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s: bad answer: %w", method, err)
	}
	if msg.Error != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s: %s", method, msg.Error.Message)
	}
	return msg.Result, resp.StatusCode, nil
}

// sseResult picks the event carrying the answer to id (else the last one).
func sseResult(data []byte, id int) []byte {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := []byte(strings.TrimSpace(line[5:]))
		if len(payload) == 0 {
			continue
		}
		last = payload
		var head struct {
			ID *int `json:"id"`
		}
		if json.Unmarshal(payload, &head) == nil && head.ID != nil && *head.ID == id {
			return payload
		}
	}
	return last
}

func formatToolResult(res json.RawMessage) json.RawMessage {
	var r struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return toolError("Failed to parse tool result JSON")
	}
	var texts []string
	for _, c := range r.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	if r.IsError {
		msg := strings.Join(texts, " ")
		if msg == "" {
			msg = "Tool execution failed"
		}
		return toolError(msg)
	}
	out := map[string]any{"result": strings.Join(texts, "\n")}
	if len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null" {
		out["structuredContent"] = r.StructuredContent
	}
	data, _ := json.Marshal(out)
	return data
}

func toolError(msg string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"status": "error", "message": msg})
	return data
}
