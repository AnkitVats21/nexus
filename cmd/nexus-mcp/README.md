# nexus-mcp

`nexus-mcp` is a lightweight Model Context Protocol (MCP) server written in Go that equips voice assistants (such as the ESP32 Nexus assistant) with remote data retrieval skills over Streamable HTTP.

Hardware skills (volume, timers, alarms, display) remain in device firmware; data skills (news, real-time web search, and weather) run on this server so that new skills and integrations require no firmware reflashing.

## Features

- **Official Go MCP SDK**: Built with `github.com/modelcontextprotocol/go-sdk` using the Streamable HTTP transport on `/mcp`.
- **JSON-Preferred Protocol**: Supports clients sending `Accept: application/json, text/event-stream` and returns compact `application/json` responses for low-memory microcontrollers.
- **Constant-Time Authentication**: Requires `Authorization: Bearer <token>` on all requests.
- **Voice-Optimized Tools**: Concise text output formatted for text-to-speech synthesis alongside structured JSON fields (under 4 KB per result):
  1. `news_headlines`: Google News RSS (top stories, topics, search queries).
  2. `web_search`: Google-grounded search via Gemini 2.5 Flash (`generateContent`).
  3. `weather`: Open-Meteo current conditions and multi-day forecast with WMO weather codes and in-memory geocoding cache.
- **Device-only tools** (marked `_meta: {"nexus/device_only": true}`; the device calls them itself and does not offer them to the voice model):
  1. `music_search`: songs for a query (`tracks`: id, title, artist, duration).
  2. `music_track`: one song by `id` or `query` (top result): details, the best Opus `stream_url`, and up to `recommendations` related songs. A video that is gone returns `error: "not_found"` (not a tool error), so the device skips it.
- **Robustness**: 8-second outbound HTTP timeouts; returns protocol-level `isError` tool results instead of panicking.

## Use as a library

The module path is `github.com/AnkitVats21/nexus-mcp`. The `tools` package
reads no environment: the caller passes the config in, and the standalone
server (`main.go`) is one such caller. One registry lists the tools and
registers them on the MCP server, so an in-process caller (nexus-hub) sees
exactly what the server serves:

```go
reg := tools.NewDefaultRegistry(tools.Config{
    GeminiKeyFile: "/path/to/gemini_key", // or GeminiAPIKey; web_search needs one
    MusicURL:      "https://music.example", // music_search / music_track
    // HTTPClient, MusicClient: optional
})
for _, t := range reg.Tools() {
    // t.Name, t.Description, t.InputSchema (JSON schema), t.DeviceOnly
}
tool, _ := reg.Get("weather")
res, err := tool.Call(ctx, json.RawMessage(`{"location":"London"}`))
// res.Text (voice readout), res.Structured (JSON), res.IsError
// err is only for arguments that do not decode.

reg.Register(mcpServer) // the same tools on an *mcp.Server
```

nexus-hub depends on it with a `replace github.com/AnkitVats21/nexus-mcp =>
../nexus-mcp`; this server stays the tool source for the board when the hub is
away.

## Environment Variables

The server reads these in `main.go` and passes them to the tools.

| Variable | Required | Default | Description |
|---|---|---|---|
| `NEXUS_MCP_TOKEN` | **Yes** | — | Bearer token for client authentication (refuses to start if unset) |
| `NEXUS_MCP_ADDR` | No | `0.0.0.0:8787` | Bind address and port for the HTTP server |
| `GEMINI_API_KEY` | No | — | Google Gemini API key for `web_search` |
| `GEMINI_API_KEY_FILE` | No | — | Path to a file containing the Gemini API key |
| `NEXUS_MCP_MUSIC_API` | No | `https://stream.ankitm.xyz` | Invidious-compatible API (invidious-daemon) for the device's music lookups; its stream URLs must be resolved from the device's network |

> **Security Note**: Never commit API keys or tokens into version control. `nexus-mcp` never logs or prints credentials.

## Build and Run

### Prerequisites
- Go 1.25 or newer

### Building

```bash
# Build the server binary
go build -o nexus-mcp .

# Build the verification CLI tool
go build -o mcpcheck ./cmd/mcpcheck
```

### Running Locally

```bash
export NEXUS_MCP_TOKEN="choose-a-strong-random-token"
export NEXUS_MCP_ADDR="127.0.0.1:8787"
export GEMINI_API_KEY_FILE="/path/to/gemini_key"

./nexus-mcp
```

### Running Tests

```bash
# Run all unit and integration tests (uses stubbed HTTP servers, no network required)
go test -v ./...
```

## Testing with `mcpcheck`

A standalone CLI tool `cmd/mcpcheck` is included for verifying server functionality:

```bash
# List available tools
./mcpcheck -endpoint "http://127.0.0.1:8787/mcp" -token "choose-a-strong-random-token"

# Test news headlines
./mcpcheck -endpoint "http://127.0.0.1:8787/mcp" -token "choose-a-strong-random-token" \
  -tool "news_headlines" -args '{"limit":3,"topic":"technology"}'

# Test weather
./mcpcheck -endpoint "http://127.0.0.1:8787/mcp" -token "choose-a-strong-random-token" \
  -tool "weather" -args '{"location":"London","days":1}'

# Test web search
./mcpcheck -endpoint "http://127.0.0.1:8787/mcp" -token "choose-a-strong-random-token" \
  -tool "web_search" -args '{"query":"latest Mars rover updates"}'
```

## Production Deployment

`nexus-mcp` serves plain HTTP; TLS termination should be handled by a reverse proxy.

### Systemd Unit (`/etc/systemd/system/nexus-mcp.service`)

```ini
[Unit]
Description=Nexus MCP Remote Data Server
After=network.target

[Service]
Type=simple
User=nexus
Group=nexus
WorkingDirectory=/opt/nexus-mcp
ExecStart=/opt/nexus-mcp/nexus-mcp
Restart=always
RestartSec=5s

# Security sandboxing
ProtectSystem=full
ProtectHome=read-only
NoNewPrivileges=true

# Environment configuration
Environment="NEXUS_MCP_ADDR=127.0.0.1:8787"
EnvironmentFile=/etc/nexus-mcp/env

[Install]
WantedBy=multi-user.target
```

Environment file `/etc/nexus-mcp/env` (permissions `0600`):
```ini
NEXUS_MCP_TOKEN=your-random-secret-token
GEMINI_API_KEY_FILE=/etc/nexus-mcp/gemini_key
```

### Reverse Proxy Configuration

#### Caddy

```caddy
mcp.your-domain.example {
    reverse_proxy 127.0.0.1:8787 {
        header_up Host {host}
        header_up X-Real-IP {remote}
    }
}
```

#### Nginx

```nginx
server {
    server_name mcp.your-domain.example;
    listen 443 ssl http2;

    ssl_certificate /etc/letsencrypt/live/mcp.your-domain.example/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mcp.your-domain.example/privkey.pem;

    location /mcp {
        proxy_pass http://127.0.0.1:8787;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Disable response buffering for streaming support
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 60s;
    }
}
```

## License

MIT
