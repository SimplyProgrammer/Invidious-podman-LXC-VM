#!/usr/bin/env bash
set -euo pipefail

STACK_DIR="$(pwd)"
SERVICE_NAME="invidious"
DEFAULT_REPO="https://github.com/iv-org/invidious.git"
CADDYFILE="${STACK_DIR}/Caddyfile"

echo "==== Invidious Setup + Caddy internal TLS ===="

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
  git clone --depth 1  "$REPO_URL" invidious
fi

# 2) Ask for DNS and gen Caddyfile
if [ -f "$CADDYFILE" ]; then
  echo "[*] Caddyfile already exists."
else
  DFLT_DOMAIN="$(hostname 2>/dev/null || echo 'invidious').home.arpa"
  read -rp "Enter DNS domain for HTTPS [${DFLT_DOMAIN}]: " DOMAIN
  DOMAIN="${DOMAIN:-$DFLT_DOMAIN}"

  DOMAIN="${DOMAIN#http://}"
  DOMAIN="${DOMAIN#https://}"

  # Intermediate CA lifetime in the global pki block (120d) expressed in hours
  INTERMEDIATE_HOURS=$((120 * 24))

  read -rp "Cert expire time in hours [0]: " CERT_HOURS
  CERT_HOURS="${CERT_HOURS:-0}"

  if ! [[ "$CERT_HOURS" =~ ^[0-9]+$ ]]; then
    echo "[!] Cert expire time must be a non-negative integer."
    exit 1
  fi

  # Force base 10 (avoids "08" being treated as octal)
  CERT_HOURS=$((10#$CERT_HOURS))

  if [ "$CERT_HOURS" -ge "$INTERMEDIATE_HOURS" ]; then
    echo "[!] Cert lifetime must be shorter than the intermediate CA lifetime (${INTERMEDIATE_HOURS}h)."
    exit 1
  fi

  # Optional tls block (only if HOURS > 0). Note: the trailing blank line
  # inside the quotes is intentional so reverse_proxy lands on its own line.
  TLS_BLOCK=""
  if [ "$CERT_HOURS" -gt 0 ]; then
    TLS_BLOCK="    tls {
        issuer internal {
            ca local
            lifetime ${CERT_HOURS}h
        }
    }

"
  fi

  echo "[*] Writing Caddyfile for: ${DOMAIN}"

  cat > "$CADDYFILE" <<EOF
{
    #debug

    auto_https disable_redirects

    pki {
        ca local {
            name "Local Invidious CA"
            root_cn "Local Invidious Root CA"
            intermediate_cn "Local Invidious Intermediate CA"
            intermediate_lifetime 120d
        }
    }
}

:80 {
    header {
        -Server
        -Via
    }

    redir https://${DOMAIN}{uri} 308
}

:443 {
    header {
        -Server
        -Via
    }

    respond "Not Found" 404
}

(ivds_site) {
${TLS_BLOCK}    reverse_proxy invidious-app:3000 {
        header_down -Server
        header_down -Via
    }

    encode gzip zstd

    header {
        -Server
        -Via
    }
}

${DOMAIN} {
    import ivds_site
}
EOF
fi

# 3) Install dependencies
inst() {
    if command -v apk >/dev/null 2>&1; then
        apk update
        apk add "$@"
    elif command -v apt-get >/dev/null 2>&1; then
        apt-get update
        apt-get install -y "$@"
    elif command -v apt >/dev/null 2>&1; then
        apt update
        apt install -y "$@"
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y "$@"
    elif command -v yum >/dev/null 2>&1; then
        yum install -y "$@"
    elif command -v pacman >/dev/null 2>&1; then
        pacman -Sy --noconfirm "$@"
    elif command -v zypper >/dev/null 2>&1; then
        zypper --non-interactive install "$@"
    elif command -v xbps-install >/dev/null 2>&1; then
        xbps-install -Sy "$@"
    elif command -v emerge >/dev/null 2>&1; then
        emerge "$@"
    elif command -v opkg >/dev/null 2>&1; then
        opkg update
        opkg install "$@"
    else
        echo "Unsupported package manager."
        exit 1
    fi
}

inst podman podman-compose pwgen git ca-certificates curl

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
SERVICE_PATH="/etc/systemd/system/${SERVICE_NAME}.service"

echo "[*] Writing systemd service..."
cat > "$SERVICE_PATH" <<EOF
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
systemctl enable --now "${SERVICE_NAME}.service"

echo
echo "==== Setup Complete ===="
echo "Access via: https://<your-domain> (default: https://invidious.home.arpa)"
echo
echo "To trust Caddy internal CA:"
echo "podman volume inspect invidious-podman_caddy_data --format '{{.Mountpoint}}'"
echo
