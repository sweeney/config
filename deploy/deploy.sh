#!/usr/bin/env bash
#
# Build and deploy the config service to a remote host.
#
# Usage:
#   ./deploy/deploy.sh sweeney@garibaldi
#
# Keeps the last 3 versioned binaries in /opt/config/bin/ and symlinks
# the active one. Restarts the config service after upload.
# Requires passwordless sudo for systemctl on the remote.
#
# First-time setup: run deploy/install.sh on the target host with sudo.
#
# Environment overrides:
#   HEALTH_URL   config /healthz URL (default https://config.swee.net/healthz)
#
set -euo pipefail

REMOTE="${1:?Usage: $0 user@host}"
BINARY="config-server"
BUILD_DIR="bin"
DEPLOY_DIR="/opt/config/bin"

# Verify the target deploy directory exists. If not, install.sh has not been
# run on the remote yet — see docs/deployment.md for the migration path.
if ! ssh "$REMOTE" "test -d $DEPLOY_DIR"; then
    echo "ERROR: $DEPLOY_DIR does not exist on $REMOTE"
    echo ""
    echo "The production server may still be using the bootstrap layout"
    echo "(binaries in /opt/identity/bin/, user=identity)."
    echo ""
    echo "See docs/deployment.md for:"
    echo "  - How to do manual deploys in the bootstrap layout"
    echo "  - How to migrate to the target layout and run install.sh"
    exit 1
fi
HEALTH_PORT="${HEALTH_PORT:-8282}"
KEEP_VERSIONS=3

VERSION=$(date +%Y%m%d-%H%M%S)
COMMIT=$(git rev-parse --short HEAD)
REMOTE_BIN="${BINARY}-${VERSION}"

echo "=== Building $BINARY (linux/amd64) ==="
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=${COMMIT}" -o "$BUILD_DIR/$BINARY" ./cmd/server/
echo "  Built: $BUILD_DIR/$BINARY"

echo "=== Uploading to $REMOTE ==="
scp "$BUILD_DIR/$BINARY" "$REMOTE:$DEPLOY_DIR/$REMOTE_BIN"
ssh "$REMOTE" "chmod 755 $DEPLOY_DIR/$REMOTE_BIN"

echo "=== Activating $REMOTE_BIN ==="
ssh "$REMOTE" "ln -sfn $REMOTE_BIN $DEPLOY_DIR/$BINARY"

echo "=== Restarting config ==="
ssh "$REMOTE" "sudo systemctl restart config"

echo "=== Verifying ==="
sleep 2

if ssh "$REMOTE" "sudo systemctl is-active --quiet config"; then
    echo "  ✓ config is running"
else
    echo "  ✗ config failed to start"
    ssh "$REMOTE" "sudo journalctl -u config -n 20 --no-pager"
    exit 1
fi

ADVERTISED=$(ssh "$REMOTE" "curl -sf http://localhost:${HEALTH_PORT}/healthz" | grep -o '"version":"[^"]*"' | cut -d'"' -f4)
if [ "$ADVERTISED" = "$COMMIT" ]; then
    echo "  ✓ config version $COMMIT confirmed via localhost:${HEALTH_PORT}"
else
    echo "  ✗ config version mismatch: deployed $COMMIT but localhost:${HEALTH_PORT} reports '${ADVERTISED:-<no response>}'"
    exit 1
fi

echo "=== Cleaning old versions (keeping $KEEP_VERSIONS) ==="
ssh "$REMOTE" "\
  cd $DEPLOY_DIR && \
  ls -t ${BINARY}-* \
    | tail -n +$((KEEP_VERSIONS + 1)) \
    | xargs -r rm --"

echo ""
echo "=== Deployed $VERSION ==="
ssh "$REMOTE" "sudo journalctl -u config -n 5 --no-pager"
