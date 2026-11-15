#!/bin/sh
set -eu

STACK_DIR="$(pwd)"
SERVICE_NAME="invidious"
DEFAULT_REPO="https://github.com/iv-org/invidious.git"
CADDY_DIR="${STACK_DIR}/caddy-config"
CADDYFILE="${CADDY_DIR}/Caddyfile"
ADAPTIVE_FILE="${CADDY_DIR}/adaptive-proxy.caddy"

echo "==== Invidious Setup + Caddy internal TLS ===="

cd "$STACK_DIR" || {
  echo "[!] Stack dir not found: $STACK_DIR"
  exit 1
}

# 1) Ask for repository
printf 'Enter Invidious repo URL [%s]: ' "$DEFAULT_REPO"
read -r REPO_URL
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
  mkdir -p "$CADDY_DIR"

  DFLT_DOMAIN="$(hostname 2>/dev/null || echo 'invidious').home.arpa"
  printf 'Enter DNS domain for HTTPS [%s]: ' "$DFLT_DOMAIN"
  read -r DOMAIN
  DOMAIN="${DOMAIN:-$DFLT_DOMAIN}"

  DOMAIN="${DOMAIN#http://}"
  DOMAIN="${DOMAIN#https://}"

  # Intermediate CA lifetime in the global pki block (120d) expressed in hours
  INTERMEDIATE_HOURS=$((120 * 24))

  printf 'Cert expire time in hours [0]: '
  read -r CERT_HOURS
  CERT_HOURS="${CERT_HOURS:-0}"

  case "$CERT_HOURS" in
    *[!0-9]*)
      echo "[!] Cert expire time must be a non-negative integer."
      exit 1
      ;;
  esac

  # Strip leading zeros (e.g. "08" -> "8"), empty result means 0
  while [ "${CERT_HOURS#0}" != "$CERT_HOURS" ]; do
    CERT_HOURS="${CERT_HOURS#0}"
  done
  CERT_HOURS="${CERT_HOURS:-0}"

  if [ "$CERT_HOURS" -ge "$INTERMEDIATE_HOURS" ]; then
    echo "[!] Cert lifetime must be shorter than the intermediate CA lifetime (${INTERMEDIATE_HOURS}h)."
    exit 1
  fi

  # Optional tls block (only if HOURS > 0). Note: the trailing blank line
  # inside the quotes is intentional so the import lands on its own line.
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

  # Alternative backends for the adaptive proxy
  DFLT_BACKENDS="https://inv-us1.nadeko.net https://inv-cl2.nadeko.net https://yt-comp2.chocolatemoo53.com https://eu.yt-comp4.chocolatemoo53.com https://invidious.nerdvpn.de https://eu.yt-comp6.chocolatemoo53.com"

  echo
  echo "Alternative backends (adaptive proxy):"
  echo "  Video playback requests can be spread across"
  echo "  the local Invidious and other public instances. Backend is picked by"
  echo "  weighted random based on its latency and errors, failing ones are"
  echo "  temporarily skipped. The local backend is always included."
  echo "  Enter URLs separated by spaces, or '-' for none (local only)."
  echo "  NOTE: the defaults are public instances and might stop working in the future."
  echo "  Defaults:"
  for b in $DFLT_BACKENDS; do
    echo "    $b"
  done
  printf 'Alternative backends [defaults]: '
  read -r ALT_BACKENDS
  ALT_BACKENDS="${ALT_BACKENDS:-$DFLT_BACKENDS}"

  # Build backend blocks (be1..beN); empty if '-'
  BACKEND_BLOCKS=""
  if [ "$ALT_BACKENDS" != "-" ]; then
    i=1
    set -f   # no globbing while word-splitting the URL list
    for url in $ALT_BACKENDS; do
      url="${url%/}"
      case "$url" in
        http://?*|https://?*) ;;
        *)
          echo "[!] Invalid backend URL: $url"
          exit 1
          ;;
      esac
      BACKEND_BLOCKS="${BACKEND_BLOCKS}        backend be${i} ${url} {
            penalty 0
        }

"
      i=$((i + 1))
    done
    set +f
  fi

  echo "[*] Writing adaptive-proxy.caddy"

  cat > "$ADAPTIVE_FILE" <<EOF
@adaptive_paths path /companion/videoplayback* /_adaptive/status

handle @adaptive_paths {
    adaptive_proxy {
        selection adaptive_weighted_random

        max_time_to_response 7s
        latency_threshold 4s
        latency_penalty 1x
        error_penalty 2

        recovery_interval 30m
        recovery_amount 1

        max_attempts auto
        pass_through_last_error true

        header_up -Via
        header_up -Forwarded
        header_up -X-Forwarded-*
        header_up -X-Real-IP
        header_down -Server
        header_down -Via

        backend local http://invidious-app:3000 {
            penalty 0
        }

${BACKEND_BLOCKS}        circuit_breaker {
            enabled true
            failure_threshold 3
            cooldown 80s
        }

        statistics {
            enabled false
            path /_adaptive/status
            allowed_ips 127.0.0.1
        }
    }
}
EOF

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
${TLS_BLOCK}    import adaptive-proxy.caddy

    handle {
        reverse_proxy invidious-app:3000 {
            header_down -Server
            header_down -Via
        }
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

podman image prune -f

echo
echo "==== Setup Complete ===="
echo "Access via: https://<your-domain> (default: https://invidious.home.arpa)"
echo
echo "To trust Caddy internal CA:"
echo "podman volume inspect invidious-podman_caddy_data --format '{{.Mountpoint}}'"
echo