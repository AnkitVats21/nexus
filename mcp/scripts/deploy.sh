#!/usr/bin/env bash
set -euo pipefail

TARGET_HOST="${LIGHTSAIL_HOST:-}"
TARGET_USER="${LIGHTSAIL_USER:-ubuntu}"

if [ -z "$TARGET_HOST" ]; then
    echo "Error: LIGHTSAIL_HOST is required (e.g. export LIGHTSAIL_HOST=<host-or-ip>)"
    exit 1
fi

echo "==> 1. Building static Linux binary (amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o nexus-mcp .

echo "==> 2. Transferring binary to ${TARGET_USER}@${TARGET_HOST}..."
scp nexus-mcp "${TARGET_USER}@${TARGET_HOST}:/tmp/nexus-mcp"

echo "==> 3. Installing binary and restarting systemd service..."
ssh "${TARGET_USER}@${TARGET_HOST}" "sudo mv /tmp/nexus-mcp /usr/local/bin/nexus-mcp && sudo chmod 755 /usr/local/bin/nexus-mcp && sudo systemctl restart nexus-mcp"

echo "==> 4. Verifying service status..."
ssh "${TARGET_USER}@${TARGET_HOST}" "sudo systemctl is-active nexus-mcp && curl -s -f http://127.0.0.1:8788/health >/dev/null && echo 'nexus-mcp is healthy!'"

echo "==> Deployed successfully!"
