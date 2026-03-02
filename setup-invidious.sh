#!/usr/bin/env bash
set -euo pipefail

STACK_DIR="$(pwd)"
SERVICE_NAME="invidious"
DEFAULT_REPO="https://github.com/iv-org/invidious.git"
CADDYFILE="${STACK_DIR}/Caddyfile"

need_cmd() { command -v "$1" >/dev/null 2>&1; }

echo "==== Invidious Setup + Caddy internal TLS) ===="

cd "$STACK_DIR" || {
  echo "[!] Stack dir not found: $STACK_DIR"
  exit 1
}

# 1) Ask for repository
read -rp "Enter Invidious repo URL [${DEFAULT_REPO}]: " REPO_URL
REPO_URL="${REPO_URL:-$DEFAULT_REPO}"

echo "[*] Using repo: $REPO_URL"

if [ -d "invidious" ]; then
  echo "[*] 'invidious' directory already exists, skipping clone."
else
  echo "[*] Cloning repository..."
  git clone "$REPO_URL" invidious
fi

# 2) Ask for DNS and gen Caddyfile
if [ -f "$CADDYFILE" ]; then
  echo "[*] Caddyfile already exists."
else
  DFLT_DOMAIN="invidious.home.arpa"
  read -rp "Enter DNS domain for HTTPS [${DFLT_DOMAIN}]: " DOMAIN
  DOMAIN="${DOMAIN:-$DFLT_DOMAIN}"

  DOMAIN="${DOMAIN#http://}"
  DOMAIN="${DOMAIN#https://}"

  echo "[*] Writing Caddyfile for: ${DOMAIN}"

  cat > "$CADDYFILE" <<EOF
(ivds_site) {
  tls {
    issuer internal
  }
  reverse_proxy invidious-app:3000
  encode gzip zstd
}

${DOMAIN} {
  import ivds_site
}
EOF
fi

# 3) Install dependencies
PKGS_COMMON="pwgen ca-certificates curl"

if need_cmd apk; then
  echo "[*] Detected Alpine (apk). Installing packages..."
  apk update

  apk add --no-cache $PKGS_COMMON podman || {
    echo "[!] Failed to install 'podman' via apk. Ensure Alpine repos include podman."
    exit 1
  }

  if apk info -e podman-compose >/dev/null 2>&1; then
    apk add --no-cache podman-compose
  else
    echo "[*] 'podman-compose' package not found in apk repos; installing via pip..."
    apk add --no-cache py3-pip python3
    pip3 install --no-cache-dir --break-system-packages podman-compose
  fi
elif need_cmd apt-get; then
  echo "[*] Detected Debian/Ubuntu (apt-get). Installing packages..."
  apt-get update -y
  apt-get install -y podman podman-compose $PKGS_COMMON
else
  echo "[!] No supported package manager found (need apk or apt-get). Install packages manually:"
  echo "    podman, podman-compose, pwgen, ca-certificates, curl"
  exit 1
fi

# 4) Create .env if missing
ENV_FILE="${STACK_DIR}/.env"

if [ -f "$ENV_FILE" ]; then
  echo "[*] .env already exists, leaving untouched."
else
  echo "[*] Generating .env (companion=16 chars, hmac=32 chars)..."
  umask 077
  COMPANION_KEY="$(pwgen 16 1)"
  HMAC_KEY="$(pwgen 32 1)"
  cat > "$ENV_FILE" <<EOF
INVIDIOUS_COMPANION_KEY=${COMPANION_KEY}
INVIDIOUS_HMAC_KEY=${HMAC_KEY}
EOF
  chmod 600 "$ENV_FILE"
fi

# 6) Ensure scripts executable
chmod 750 start-invidious.sh stop-invidious.sh update-invidious.sh

# 7) Create systemd service
SYSTEMD_SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
OPENRC_SERVICE_PATH="/etc/init.d/${SERVICE_NAME}"

if need_cmd systemctl && [ -d /run/systemd/system ]; then
  echo "[*] Detected systemd. Writing unit file..."
  mkdir -p /etc/systemd/system

  cat > "$SYSTEMD_SERVICE_PATH" <<EOF
[Unit]
Description=Invidious podman + Caddy
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
WorkingDirectory=${STACK_DIR}
RemainAfterExit=yes
ExecStart=${STACK_DIR}/start-invidious.sh
ExecStop=${STACK_DIR}/stop-invidious.sh
TimeoutStartSec=0

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "${SERVICE_NAME}.service"
  echo "[*] Enabled systemd service: ${SERVICE_NAME}.service"

elif [ -d /etc/init.d ] && need_cmd rc-service; then
  echo "[*] Detected OpenRC. Writing init script..."

  cat > "$OPENRC_SERVICE_PATH" <<'EOF'
#!/sbin/openrc-run
name="invidious podman + caddy"
description="Invidious podman + Caddy"
command="/bin/sh"
command_args="-c \"${STACK_DIR}/start-invidious.sh\""
command_stop="/bin/sh"
command_stop_args="-c \"${STACK_DIR}/stop-invidious.sh\""
command_background="no"
pidfile="/run/invidious-podman-caddy.pid"

depend() {
  need net
  after firewall
}
EOF

  sed -i "s|\${STACK_DIR}|${STACK_DIR}|g" "$OPENRC_SERVICE_PATH"

  chmod +x "$OPENRC_SERVICE_PATH"
  rc-update add "$SERVICE_NAME" default
  echo "[*] Enabled OpenRC service: ${SERVICE_NAME}"

else
  echo "[!] No supported init system found (systemd/OpenRC)."
  echo "    You can still run manually:"
  echo "    ${STACK_DIR}/start-invidious.sh"
  exit 1
fi

echo
echo "==== Setup Complete ===="
echo "Access via: https://<your-domain> (default: https://invidious.home.arpa)"
echo
echo "To trust Caddy internal CA:"
echo "podman volume inspect caddy_data --format '{{.Mountpoint}}'"
echo
