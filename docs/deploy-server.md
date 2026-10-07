# Server Deployment Runbook: Nexus Hub on Lightsail

This runbook guides deploying `nexus-hub` onto an AWS Lightsail (or similar Ubuntu/Debian) server for the remote hub pilot.

> **Security Note**: Never commit real tokens, keys, passwords, or public IP addresses. This document uses placeholders (`hub.example.com`, `stream.example.com`).

---

## 1. Architecture Overview

```
[ ESP32 Board / Orbit Satellite ] (Home)
                 │  wss://hub.example.com/hub/board (or /hub/edge)
                 ▼
       [ nginx reverse proxy ] (:443 with TLS certbot)
                 │  http://127.0.0.1:8380
                 ▼
       [ nexus-hub daemon ] (systemd, user: nexus, --resolve-only)
         ├── Gemini Live (Voice sessions)
         └── Invidious / MCP Resolver (Song URL resolution only, egress from home)
```

- **`--resolve-only`**: The server hub resolves tracks to direct CDN URLs for players, but never downloads, caches, or streams audio bytes.
- **TLS & WebSockets**: Managed by nginx (`/hub/edge` and `/hub/board` bypass basic auth; edges authenticate via their hello token).
- **Dashboard & API Protection**: Basic auth (`/etc/nginx/.htpasswd-hub`) guards `/`, `/hub/status`, and dashboard assets. Note that during the pilot, the daemon runs without `--web`, so `/` is not served; basic auth protects `/hub/status`.

---

## 2. Server Prerequisites

On the target server:

```bash
# 1. Update packages and install build dependencies
sudo apt-get update
sudo apt-get install -y build-essential pkg-config libopus-dev nginx certbot python3-certbot-nginx apache2-utils

# 2. Install Go (v1.25+ required)
# Lightsail instances may be x86_64 (amd64) or aarch64 (arm64); detect architecture:
ARCH=$(uname -m)
case "$ARCH" in
  x86_64) GO_ARCH="amd64" ;;
  aarch64|arm64) GO_ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

curl -fsSL "https://go.dev/dl/go1.25.4.linux-${GO_ARCH}.tar.gz" | sudo tar -C /usr/local -xz
export PATH=/usr/local/go/bin:$PATH
echo 'export PATH=/usr/local/go/bin:$PATH' | sudo tee -a /etc/profile

# 3. Create dedicated system user
sudo useradd -m -s /bin/bash nexus
sudo mkdir -p /home/nexus/bin /home/nexus/src /home/nexus/.config/nexus
```

---

## 3. Transfer Source & Build on Server

Because `nexus-hub` links `libopus` via cgo for low-latency audio processing, build the binary directly on the server.

### A. Export & Transfer from Local Workstation
From your local `nexus` repository checkout:

```bash
# Export working tree without git history or secrets
git archive --format=tar.gz -o /tmp/nexus-source.tar.gz HEAD

# Copy archive to the server
scp /tmp/nexus-source.tar.gz user@hub.example.com:/tmp/
rm /tmp/nexus-source.tar.gz
```

### B. Extract and Build on Server
On the server:

```bash
# Extract into /home/nexus/src
sudo tar -xzf /tmp/nexus-source.tar.gz -C /home/nexus/src
sudo rm /tmp/nexus-source.tar.gz

# Build nexus-hub binary
sudo -u nexus bash -c 'cd /home/nexus/src && CGO_ENABLED=1 go build -o /home/nexus/bin/nexus-hub ./cmd/nexus-hub'

# Verify binary
/home/nexus/bin/nexus-hub --help
```

---

## 4. Secrets Configuration (Permissions 0600)

> **IMPORTANT**: Secrets must never land in shell history (`.bash_history`) or command line arguments. Do not use `echo "KEY" | sudo tee ...`.

Create configuration files with strict permissions:

```bash
# Ensure directory exists with 0700 permissions owned by nexus
sudo mkdir -p /home/nexus/.config/nexus
sudo chown -R nexus:nexus /home/nexus/.config
sudo chmod 700 /home/nexus/.config /home/nexus/.config/nexus

# 1. Generate Hub token directly into file with umask 077 (32-byte hex)
sudo -u nexus sh -c 'umask 077; openssl rand -hex 32 > /home/nexus/.config/nexus/hub_tokens'

# 2. Enter Gemini API key without echoing to terminal or history
echo "Enter Gemini API Key (input will not echo):"
sudo -u nexus sh -c 'umask 077; read -rs k; printf "%s" "$k" > /home/nexus/.config/nexus/gemini_key'
# (Alternative: create /tmp/key with nano/vi and run: sudo install -m 600 -o nexus /tmp/key /home/nexus/.config/nexus/gemini_key && rm /tmp/key)

# 3. Verify file permissions (must be mode 0600 owned by nexus)
sudo chmod 600 /home/nexus/.config/nexus/hub_tokens
sudo chmod 600 /home/nexus/.config/nexus/gemini_key
```

### Reading the Server Token Once for Edge Configuration
To configure Orbit and the board, read the generated token once:
```bash
sudo cat /home/nexus/.config/nexus/hub_tokens
```
> **CAUTION**: Never paste this token in chat messages, issue trackers, or git commits.

---

## 5. Basic Authentication Setup

Protect the web dashboard and status endpoints:

```bash
# Create htpasswd file for nginx basic auth
sudo htpasswd -c /etc/nginx/.htpasswd-hub pilotadmin
sudo chmod 640 /etc/nginx/.htpasswd-hub
sudo chown root:www-data /etc/nginx/.htpasswd-hub
```

---

## 6. Nginx & TLS Configuration

1. Install the site configuration:
```bash
sudo cp /home/nexus/src/deploy/nginx-hub.conf /etc/nginx/sites-available/hub.example.com.conf
sudo ln -sf /etc/nginx/sites-available/hub.example.com.conf /etc/nginx/sites-enabled/
```

2. Replace placeholder domain with your actual hostname in `/etc/nginx/sites-available/hub.example.com.conf`.

3. Obtain Let's Encrypt TLS certificate:
```bash
sudo certbot --nginx -d hub.example.com
```

4. Verify nginx configuration and reload:
```bash
sudo nginx -t
sudo systemctl reload nginx
```

---

## 7. Resolver Check & Systemd Service Setup

### Invidious Resolver Check
Because `nexus-hub` runs with `--resolve-only`, `--invidious-url` must point at the resolver whose egress is the home IP (YouTube blocks or throttles datacenter IPs like AWS Lightsail).

Before starting the service, test that the resolver is reachable and answering lookups from the server:

```bash
# Test a known video lookup on the resolver
curl -fsS -o /dev/null -w "%{http_code}\n" "https://stream.example.com/api/v1/videos/dQw4w9WgXcQ"
```

Confirm this command prints `200`.
- If the resolver fails or returns an error, **music playback resolution will be unavailable**, but voice assistant sessions and Gemini Live will still function normally.

### Service Activation
1. Edit `/home/nexus/src/deploy/nexus-hub.service` to specify your actual resolver URL for `--invidious-url=...`.
2. Copy and activate the systemd service:
```bash
sudo cp /home/nexus/src/deploy/nexus-hub.service /etc/systemd/system/nexus-hub.service
sudo systemctl daemon-reload
sudo systemctl enable --now nexus-hub
```

3. Check status and logs:
```bash
sudo systemctl status nexus-hub
journalctl -u nexus-hub -f
```

---

## 8. Point the Edges at the Server

### A. Orbit Satellite (Home PC / Workstation)
Orbit requires the server's authentication token. Keep your home hub token file intact and create a separate file for the remote server token:

```bash
# 1. Create a dedicated server token file on the home machine (mode 600)
(umask 077 && cat << 'EOF' > ~/.config/nexus/server_hub_token
<PASTE_SERVER_TOKEN_HERE>
EOF
)
chmod 600 ~/.config/nexus/server_hub_token

# 2. Launch Orbit pointing to the Lightsail server
orbit --hub wss://hub.example.com/hub/edge --hub-token-file ~/.config/nexus/server_hub_token
```

### B. Waveshare ESP32 Board
Once the board firmware with `wss://` TLS support is installed:

To avoid exposing the server token in your workstation's shell history, send the configuration via a temporary JSON file:

```bash
# 1. Create a temporary config payload with umask 077
(umask 077 && cat << 'EOF' > /tmp/hub_config.json
{
  "url": "wss://hub.example.com/hub/board",
  "token": "<PASTE_SERVER_TOKEN_HERE>"
}
EOF
)

# 2. Send configuration to the board via local REST API
curl -X POST http://<board-ip>/api/hub -H "Content-Type: application/json" -d @/tmp/hub_config.json

# 3. Clean up the temporary file immediately
rm -f /tmp/hub_config.json
```

*Note*: The board validates `wss://` with the ESP certificate bundle and verifies the server's hostname. If the server is unreachable, the board automatically falls back to direct Gemini Live mode.

---

## 9. Verifying the Deployment

> **Pilot Note**: During the pilot, `nexus-hub` runs without `--web`, so no web dashboard bundle is served on `/`. Checking `/hub/status` (via basic auth) is the primary health check.

1. **HTTP Status**:
   ```bash
   curl -i -u pilotadmin:<password> https://hub.example.com/hub/status
   ```
   Should return JSON with `"version"`, `"music": true`, and connected edges with live RTT:
   ```json
   {
     "version": "2.0.0",
     "music": true,
     "edges": [
       {
         "id": "orbit-satellite",
         "rtt_ms": {
           "last": 24,
           "min": 21,
           "avg": 23.5
         }
       }
     ]
   }
   ```

2. **Reply Latency Metrics**:
   After a voice turn, `/hub/status` also exposes recent reply delivery timings (`reply_history`) recording `hub_delay_ms` and `edge_delay_ms`.

---

## 10. Rollback to Home Hub

If the pilot server link exhibits excessive latency or issues, roll back devices to the home LAN hub:

1. **Stop Server Service**:
   ```bash
   sudo systemctl stop nexus-hub
   ```

2. **Redirect Orbit**:
   Relaunch Orbit pointing back to your LAN hub using its local token:
   ```bash
   orbit --hub ws://nexus.local:8380/hub/edge --hub-token-file ~/.config/nexus/hub_tokens
   ```

3. **Redirect Waveshare Board**:
   Restore the board to the home hub via local REST API:
   ```bash
   (umask 077 && cat << 'EOF' > /tmp/hub_home.json
   {
     "url": "ws://nexus.local:8380/hub/board",
     "token": "<HOME_HUB_TOKEN>"
   }
   EOF
   )
   curl -X POST http://<board-ip>/api/hub -H "Content-Type: application/json" -d @/tmp/hub_home.json
   rm -f /tmp/hub_home.json
   ```
