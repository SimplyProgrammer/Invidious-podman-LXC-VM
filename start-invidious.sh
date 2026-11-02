#!/bin/sh

set -e

cd "$(dirname "$0")" || exit 1

if [ ! -f .env ]; then
    {
        echo "INVIDIOUS_COMPANION_KEY=$(pwgen 16 1)"
        echo "INVIDIOUS_HMAC_KEY=$(pwgen 24 1)"
    } > .env

    chmod 600 .env
fi

exec podman-compose up -d
