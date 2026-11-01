#!/bin/sh
set -e

cd "$(dirname "$0")" || exit 1
exec podman-compose down
