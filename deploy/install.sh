#!/usr/bin/env bash
#
# Install the config service on a Linux host.
# Run as root or with sudo from the directory containing these deploy files.
#
# Prerequisites: copy these files to the target host first:
#   /tmp/config-server      — the binary
#   /tmp/config.service     — systemd unit
#   /tmp/config-env.example — env file template
#
# Usage:
#   sudo bash /tmp/install.sh
#
set -euo pipefail

echo "══════════════════════════════════"
echo "  Config Service — Install"
echo "══════════════════════════════════"
echo ""

echo "=== Installing binary ==="
mkdir -p /opt/config/bin
chown "${SUDO_USER:-root}:${SUDO_USER:-root}" /opt/config/bin
systemctl stop config 2>/dev/null || true
VERSION=$(date +%Y%m%d-%H%M%S)
cp /tmp/config-server "/opt/config/bin/config-server-${VERSION}"
chmod 755 "/opt/config/bin/config-server-${VERSION}"
ln -sfn "config-server-${VERSION}" /opt/config/bin/config-server
echo "  /opt/config/bin/config-server -> config-server-${VERSION}"

echo ""
echo "=== System user ==="
if ! id config &>/dev/null; then
    useradd --system --shell /usr/sbin/nologin --home-dir /var/lib/config config
    echo "  Created user: config"
else
    echo "  User 'config' already exists"
fi

echo ""
echo "=== Directories ==="
mkdir -p /var/lib/config
chown config:config /var/lib/config
chmod 750 /var/lib/config
echo "  /var/lib/config"

echo ""
echo "=== Environment file ==="
mkdir -p /etc/config
if [ ! -f /etc/config/config.env ]; then
    cp /tmp/config-env.example /etc/config/config.env
    chown root:root /etc/config/config.env
    chmod 600 /etc/config/config.env
    echo "  Created /etc/config/config.env from template — EDIT IT before starting"
else
    echo "  /etc/config/config.env already exists — skipping"
fi

echo ""
echo "=== systemd unit ==="
cp /tmp/config.service /etc/systemd/system/config.service
systemctl daemon-reload
systemctl enable config
echo "  config.service installed and enabled"

echo ""
echo "══════════════════════════════════"
echo "  Installation complete"
echo ""
echo "  Next steps:"
echo "  1. Edit /etc/config/config.env"
echo "  2. sudo systemctl start config"
echo "  3. sudo journalctl -u config -f"
echo "══════════════════════════════════"
