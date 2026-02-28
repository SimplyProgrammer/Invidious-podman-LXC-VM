#!/bin/sh
set -e

cd "$(dirname "$0")" || exit 1

read -r -p "Do you want to update, backing up current installation is recommended? [y/N]: " response
response=$(printf '%s' "$response" | tr '[:upper:]' '[:lower:]')

if [ "$response" = "y" ] || [ "$response" = "yes" ]; then
    podman-compose pull
    podman-compose up -d
    podman image prune -f
else
    echo "Update cancelled."
fi
