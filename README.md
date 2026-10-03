# Nexus Monorepo

Private monorepo containing all host-side software for the Nexus ecosystem (running on PC, Raspberry Pi, and servers).

> **Note on Firmware**: The ESP32-S3 firmware resides in the separate `waveshare` repository and communicates with this host stack over WebSockets and HTTP.

---

## 1. Repository Layout

```
nexus/
├── go.mod                      # Single Go module ("nexus")
├── go.sum
├── cmd/
│   ├── nexus-hub/              # Hub daemon: multi-device mesh coordinator & bridge
│   ├── orbit/                  # Orbit daemon: satellite co-player with mpv/yt-dlp
│   ├── nexus-mcp/              # Standalone MCP server (HTTP/SSE)
│   ├── mcpcheck/               # CLI utility to verify MCP servers & list tools
│   └── nexus-stub/             # Mock edge stub for hub testing
├── pkg/
│   ├── hub/                    # Hub core: edge sessions, audio relay, browser proxy
│   ├── coordinator/            # Orbit coordinator: state machine & reconnect logic
│   ├── audio/                  # Audio pipeline (mpv JSON IPC)
│   ├── transport/              # WebSocket client transport & pingers
│   ├── protocol/               # Binary & JSON message protocol definitions
│   ├── resolver/               # Pluggable audio URL resolver (direct vs invidious)
│   ├── store/                  # SQLite storage, schema migrations, and user/device models
│   ├── auth/                   # Argon2id password hashing, sessions, CSRF, and rate limiting
│   └── tools/                  # Nexus MCP tool implementations & registry
├── config/                     # Configuration parsers and structures
├── web/                        # NPM workspace for frontend apps and libraries
│   ├── package.json            # Workspace root ("workspaces": ["packages/*", "apps/*"])
│   ├── packages/
│   │   └── ui/                 # @nexus/ui: shared components, formatters, and clients
│   └── apps/
│       ├── device/             # Device dashboard (waveshare-studio; bundled to ESP)
│       └── hub/                # Hub management web app (edges overview & telemetry)
├── docs/
│   └── PROTOCOL.md             # Edge-to-hub WebSocket & binary protocol specification
├── DESIGN.md                   # Orbit satellite architecture & design specification
└── scripts/
    └── nexus-mcp/              # Deployment and utility scripts
```

---

## 2. Building Go Binaries

All Go components build from the root of the repository:

```bash
# Build Hub
go build -o bin/nexus-hub ./cmd/nexus-hub

# Build Orbit Satellite (CGO disabled for portability)
CGO_ENABLED=0 go build -o bin/orbit ./cmd/orbit

# Build Standalone Nexus MCP Server
go build -o bin/nexus-mcp ./cmd/nexus-mcp

# Build MCP verification tool
go build -o bin/mcpcheck ./cmd/mcpcheck
```

### Running Tests
```bash
go test -count=1 ./...
```

---

## 3. Web Workspace (`web/`)

Frontend packages and applications are managed in an npm workspace.

### Prerequisites
```bash
nvm use node
```

### Install Dependencies
From the repository root:
```bash
cd web
npm install
```

### Building Applications
- **Device App (`web/apps/device`)**:
  The primary device control and management dashboard.
  ```bash
  cd web/apps/device
  npm run build
  ```
  *Bundle output:* Serves from `web/apps/device/dist`. Kept extremely lightweight (~414 KB) to fit flash constraints when bundled into `nexus-web.bin`.
  *Schema Generator:* `npm run ndb` references `../../../../waveshare/schema/db` to re-generate binary database schemas.

- **Hub App (`web/apps/hub`)**:
  Single-page status viewer that connects to `/hub/status` and displays connected edges and active board telemetry.
  ```bash
  cd web/apps/hub
  npm run build
  ```

---

## 4. `nexus-mcp` Dual Operating Modes

The tools package (`pkg/tools`) supports two operational modes:

1. **In-Process Library (`pkg/tools`)**:
   Used directly by `nexus-hub` via the `--local-tools` flag. The hub executes tools (weather, news, music lookups, web search) in-process with zero IPC overhead.

2. **Standalone Server (`cmd/nexus-mcp`)**:
   An MCP-compliant HTTP/SSE server that external assistants or remote boards can connect to.
   ```bash
   export NEXUS_MCP_TOKEN="your-token"
   ./bin/nexus-mcp -port 8382
   ```

---

## 5. Serving Web UI via Hub

`nexus-hub` supports serving static web interfaces using the `--web` flag:

```bash
# Today: serve the full device dashboard
./bin/nexus-hub --web web/apps/device/dist

# Future: serve the multi-edge hub management dashboard
./bin/nexus-hub --web web/apps/hub/dist
```
*(Embedded binary assets via `go:embed` will be introduced in a future release).*
