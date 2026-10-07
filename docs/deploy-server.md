# Server Deployment Runbook: Nexus Hub on Lightsail

This runbook guides deploying `nexus-hub` onto an AWS Lightsail (or similar Ubuntu/Debian) server for the remote hub pilot.

> **Security Note**: Never commit real tokens, keys, passwords, or public IP addresses. This document uses placeholders (`hub.example.com`, `stream.example.com`).

---

## 1. Architecture Overview

```
[ ESP32 Board / Orbit Satellite ] (Home)
                 │  wss://hub.example.com/hub/edge
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
- **Dashboard & API Protection**: Basic auth (`/etc/nginx/.htpasswd-hub`) guards `/`, `/hub/status`, and dashboard assets.

---

## 2. Server Prerequisites

On the target server:

```bash
# 1. Update packages and install build dependencies
sudo apt-get update
sudo apt-get install -y build-essential pkg-config libopus-dev nginx certbot python3-certbot-nginx apache2-utils

# 2. Install Go (v1.25+ required)
# If standard apt Go is older, install from official Go binaries:
curl -fsSL https://go.dev/dl/go1.25.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
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

Create configuration files with strict permissions:

```bash
# 1. Hub token file (one token per line, identical to the board's token)
# Generate a random 32-byte hex token:
NEW_TOKEN=$(openssl rand -hex 16)
echo "$NEW_TOKEN" | sudo tee /home/nexus/.config/nexus/hub_tokens > /dev/null

# 2. Gemini API key
echo "YOUR_GEMINI_API_KEY_HERE" | sudo tee /home/nexus/.config/nexus/gemini_key > /dev/null

# 3. Secure file permissions
sudo chown -R nexus:nexus /home/nexus/.config
sudo chmod 700 /home/nexus/.config /home/nexus/.config/nexus
sudo chmod 600 /home/nexus/.config/nexus/hub_tokens
sudo chmod 600 /home/nexus/.config/nexus/gemini_key
```

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

## 7. Systemd Service Setup

1. Copy and activate the service:
```bash
sudo cp /home/nexus/src/deploy/nexus-hub.service /etc/systemd/system/nexus-hub.service
sudo systemctl daemon-reload
sudo systemctl enable --now nexus-hub
```

2. Check status and logs:
```bash
sudo systemctl status nexus-hub
journalctl -u nexus-hub -f
```

---

## 8. Verifying the Deployment

1. **HTTP Status**:
   ```bash
   curl -i -u pilotadmin:<password> https://hub.example.com/hub/status
   ```
   Should return JSON with `"version"`, `"music": true`, and empty `"edges": []`.

2. **Edge Verification with Orbit**:
   From your workstation or a Raspberry Pi:
   ```bash
   orbit --hub wss://hub.example.com/hub/edge
   ```
   Check `/hub/status` on the server to verify Orbit appears in `"edges"` with live RTT:
   ```json
   {
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

---

## 9. Rollback to Home Hub

If the pilot server link exhibits excessive latency or issues, roll back devices to the home hub:

1. **Stop Server Service**:
   ```bash
   sudo systemctl stop nexus-hub
   ```

2. **Redirect Orbit**:
   Relaunch Orbit pointing back to your LAN hub:
   ```bash
   orbit --hub ws://nexus.local:8380/hub/edge
   ```

3. **Redirect Waveshare Board**:
   Send a POST request to the board's local web API:
   ```bash
   curl -X POST http://<board-ip>/api/hub -d '{"url":"ws://nexus.local:8380/hub/edge","token":"<home-token>"}'
   ```
