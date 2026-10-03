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
	"os"
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
	mcpAuthBackoff         = 30 * time.Minute
)

var mcpToolName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)

// MCPClient calls an MCP server over streamable HTTP (JSON-RPC; the answer
// is JSON or server-sent events). The hub uses it for the voice session's
// remote tools, as the board does in direct mode.
type MCPClient struct {
	url         string
	token       string
	tokenFile   string
	tokenMod    time.Time
	maxTools    int
	http        *http.Client
	log         *slog.Logger
	authBackoff time.Duration

	mu               sync.Mutex
	refreshMu        sync.Mutex
	session          string // Mcp-Session-Id
	inited           bool
	nextID           int
	tools            []mcpTool
	toolsAt          time.Time
	lastError        string
	backoffUntil     time.Time
	authLogged       bool
	emptyTokenLogged bool

	started bool
	stop    chan struct{}
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
	return &MCPClient{
		url:         url,
		token:       token,
		maxTools:    maxTools,
		http:        &http.Client{Timeout: mcpCallTimeout},
		log:         log.With("component", "mcp"),
		authBackoff: mcpAuthBackoff,
		stop:        make(chan struct{}),
	}
}

// SetTokenFile configures a token file path. When configured, token updates
// on disk are automatically loaded and clear any active auth backoff.
func (c *MCPClient) SetTokenFile(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenFile = path
	c.checkTokenFileLocked()
}

func (c *MCPClient) checkTokenFileLocked() {
	if c.tokenFile == "" {
		return
	}
	st, err := os.Stat(c.tokenFile)
	if err != nil {
		return
	}
	if !st.ModTime().After(c.tokenMod) && !c.tokenMod.IsZero() {
		return
	}
	c.tokenMod = st.ModTime()
	newToken := readSecretFile(c.tokenFile)
	if newToken != c.token {
		c.token = newToken
		// Token changed: clear auth backoff and session so requests can re-authenticate.
		c.backoffUntil = time.Time{}
		c.authLogged = false
		c.emptyTokenLogged = false
		c.inited = false
		c.session = ""
		if c.token != "" {
			c.log.Info("MCP token updated from file", "file", c.tokenFile)
		}
	}
}

func (c *MCPClient) currentTokenLocked() string {
	c.checkTokenFileLocked()
	return c.token
}

func (c *MCPClient) inBackoffLocked() bool {
	c.checkTokenFileLocked()
	if c.backoffUntil.IsZero() {
		return false
	}
	if time.Now().After(c.backoffUntil) {
		c.backoffUntil = time.Time{}
		c.authLogged = false
		return false
	}
	return true
}

// InBackoff reports whether the client is currently in auth backoff.
func (c *MCPClient) InBackoff() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inBackoffLocked()
}

// Start begins periodic background refresh of declarations (every 10 minutes).
// The initial refresh runs asynchronously so Start never blocks callers.
func (c *MCPClient) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()

	go c.refreshLoop(ctx)
}

// Close stops the periodic background refresh loop.
func (c *MCPClient) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}

func (c *MCPClient) refreshLoop(ctx context.Context) {
	_ = c.Refresh(ctx)
	ticker := time.NewTicker(mcpToolsMaxAge)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Refresh(ctx)
		}
	}
}

// Refresh fetches and updates the cached tool list from the remote MCP server.
// If the client is in auth backoff or has no token, it skips the network call.
func (c *MCPClient) Refresh(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.Lock()
	if c.inBackoffLocked() {
		c.mu.Unlock()
		return errors.New("MCP auth backoff active")
	}
	if c.currentTokenLocked() == "" {
		if !c.emptyTokenLogged {
			c.emptyTokenLogged = true
			if c.tokenFile != "" {
				c.log.Info("Remote MCP disabled: no token found", "file", c.tokenFile)
			} else {
				c.log.Info("Remote MCP disabled: no token configured")
			}
		}
		c.mu.Unlock()
		return errors.New("remote MCP disabled: no token")
	}
	// Return cached list if recently updated.
	if len(c.tools) > 0 && time.Since(c.toolsAt) < 5*time.Second {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	res, err := c.rpc(ctx, "tools/list", json.RawMessage("{}"))
	if err != nil {
		c.mu.Lock()
		logged := c.authLogged
		c.mu.Unlock()
		if !logged {
			c.log.Warn("MCP tools unavailable", "err", err)
		}
		return err
	}
	var list struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}
	c.mu.Lock()
	c.tools = list.Tools
	c.toolsAt = time.Now()
	c.mu.Unlock()
	c.log.Info("MCP tools listed", "count", len(list.Tools))
	return nil
}

// Declarations are the MCP tools as Gemini function declarations: device-only
// tools, names in skip and (with builtin search) web_search left out, then
// the board's limits on count and size. Declarations are served strictly from
// the in-memory cache and never block on network requests.
func (c *MCPClient) Declarations(ctx context.Context, skip map[string]bool, builtinSearch bool) []map[string]any {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	tools := c.tools
	c.mu.Unlock()
	if len(tools) == 0 {
		return nil
	}
	return buildDeclarations(tools, skip, builtinSearch, c.maxTools)
}

// buildDeclarations turns tool descriptions into Gemini function
// declarations, leaving out the ones the voice model is not offered:
// device-only tools, names in skip, bad names, web_search when Gemini
// searches by itself; then the board's limits on count and size.
func buildDeclarations(tools []mcpTool, skip map[string]bool, builtinSearch bool, maxTools int) []map[string]any {
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
		if len(out) >= maxTools {
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

// ToolNames lists the server's tool names (from cache, or refreshed if empty).
func (c *MCPClient) ToolNames(ctx context.Context) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	tools := c.tools
	c.mu.Unlock()
	if len(tools) == 0 {
		if err := c.Refresh(ctx); err == nil {
			c.mu.Lock()
			tools = c.tools
			c.mu.Unlock()
		}
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// Call runs a tool and returns the result as the board formats it for
// Gemini: {"result": joined text, "structuredContent"?} or {"status":"error",...}.
func (c *MCPClient) Call(ctx context.Context, name string, args json.RawMessage) json.RawMessage {
	if c == nil {
		return toolError("no MCP server")
	}
	c.mu.Lock()
	if c.inBackoffLocked() {
		c.mu.Unlock()
		return toolError("MCP auth backoff active")
	}
	if c.currentTokenLocked() == "" {
		if !c.emptyTokenLogged {
			c.emptyTokenLogged = true
			if c.tokenFile != "" {
				c.log.Info("Remote MCP disabled: no token found", "file", c.tokenFile)
			} else {
				c.log.Info("Remote MCP disabled: no token configured")
			}
		}
		c.mu.Unlock()
		return toolError("remote MCP disabled: no token")
	}
	c.mu.Unlock()

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

// CallStructured runs a tool and decodes its structured result into out
// (the device-only music tools). A tool error is an error.
func (c *MCPClient) CallStructured(ctx context.Context, name string, args json.RawMessage, out any) error {
	if c == nil {
		return errors.New("no MCP server")
	}
	c.mu.Lock()
	if c.inBackoffLocked() {
		c.mu.Unlock()
		return errors.New("MCP auth backoff active")
	}
	if c.currentTokenLocked() == "" {
		if !c.emptyTokenLogged {
			c.emptyTokenLogged = true
			if c.tokenFile != "" {
				c.log.Info("Remote MCP disabled: no token found", "file", c.tokenFile)
			} else {
				c.log.Info("Remote MCP disabled: no token configured")
			}
		}
		c.mu.Unlock()
		return errors.New("remote MCP disabled: no token")
	}
	c.mu.Unlock()

	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	res, err := c.rpc(ctx, "tools/call", params)
	if err != nil {
		return err
	}
	var r struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return fmt.Errorf("%s: bad result: %w", name, err)
	}
	if r.IsError {
		msg := name + " failed"
		if len(r.Content) > 0 && r.Content[0].Text != "" {
			msg = r.Content[0].Text
		}
		return errors.New(msg)
	}
	if len(r.StructuredContent) == 0 || string(r.StructuredContent) == "null" {
		return fmt.Errorf("%s: no structured result", name)
	}
	return json.Unmarshal(r.StructuredContent, out)
}

// rpc sends one request (initializing the session first, and again once if
// the server has forgotten it) and returns its "result".
func (c *MCPClient) rpc(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensureInit(ctx); err != nil {
			return nil, err
		}
		res, status, err := c.post(ctx, method, params, true)
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return nil, err
		}
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
	c.mu.Lock()
	if c.inBackoffLocked() {
		c.mu.Unlock()
		return nil, 0, errors.New("MCP auth backoff active")
	}
	token := c.currentTokenLocked()
	if token == "" {
		if !c.emptyTokenLogged {
			c.emptyTokenLogged = true
			if c.tokenFile != "" {
				c.log.Info("Remote MCP disabled: no token found", "file", c.tokenFile)
			} else {
				c.log.Info("Remote MCP disabled: no token configured")
			}
		}
		c.mu.Unlock()
		return nil, 0, errors.New("remote MCP disabled: no token")
	}
	c.nextID++
	id := c.nextID
	session := c.session
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "method": method}
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
	hr.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		hr.Header.Set("Mcp-Session-Id", session)
	}

	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.mu.Lock()
		dur := c.authBackoff
		if dur <= 0 {
			dur = mcpAuthBackoff
		}
		c.backoffUntil = time.Now().Add(dur)
		c.inited = false
		c.session = ""
		logged := c.authLogged
		c.authLogged = true
		c.mu.Unlock()

		if !logged {
			c.log.Warn("MCP auth failed, backing off", "status", resp.StatusCode, "backoff", dur)
		}
		return nil, resp.StatusCode, fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}

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

func readSecretFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}
