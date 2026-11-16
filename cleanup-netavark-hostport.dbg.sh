#!/bin/sh
set -eu

FAMILY=inet
TABLE=netavark
CHAIN=NETAVARK-HOSTPORT-DNAT
MODE=dry-run

case "${1-}" in
    "") ;;
    --apply) MODE=apply ;;
    --help|-h)
        echo "Usage: $0 [--apply]"
        echo "Default: dry-run. --apply: delete confirmed stale rules."
        exit 0
        ;;
    *)
        echo "Error: unknown argument: $1" >&2
        exit 2
        ;;
esac

if [ "$(id -u)" -ne 0 ]; then
    echo "Error: run this script as root." >&2
    exit 1
fi

for cmd in podman nft awk grep mktemp sed; do
    command -v "$cmd" >/dev/null 2>&1 || {
        echo "Error: required command not found: $cmd" >&2
        exit 1
    }
done

TMP_IDS=$(mktemp)
TMP_NAMES=$(mktemp)
TMP_RULES=$(mktemp)
TMP_LIVE=$(mktemp)
TMP_RAW=$(mktemp)

trap 'rm -f "$TMP_IDS" "$TMP_NAMES" "$TMP_RULES" "$TMP_LIVE" "$TMP_RAW"' 0
trap 'exit 1' HUP INT TERM

# Get network names, then inspect each network for its full ID.
if ! podman network ls --format '{{.Name}}' >"$TMP_NAMES"; then
    echo "Error: cannot list Podman networks." >&2
    exit 1
fi

: >"$TMP_IDS"

while IFS= read -r name; do
    [ -n "$name" ] || continue

    if ! network_id=$(podman network inspect --format '{{.ID}}' "$name"); then
        echo "Error: cannot inspect network '$name'." >&2
        exit 1
    fi

    case "$network_id" in
        ""|*[!0123456789abcdefABCDEF]*)
            echo "Error: unexpected network ID for '$name': '$network_id'" >&2
            exit 1
            ;;
    esac

    if [ "${#network_id}" -lt 8 ]; then
        echo "Error: network ID too short for '$name'." >&2
        exit 1
    fi

    printf '%s\n' "$(printf '%s' "$network_id" | cut -c1-8)" >>"$TMP_IDS"
done <"$TMP_NAMES"

if [ ! -s "$TMP_IDS" ]; then
    echo "Error: no Podman network IDs found; refusing to continue." >&2
    exit 1
fi

if ! nft list chain "$FAMILY" "$TABLE" "$CHAIN" >/dev/null 2>&1; then
    echo "Error: Netavark host-port chain not found." >&2
    exit 1
fi

# Extract Netavark jump rules and their current handles.
list_rules() {
    nft -a list chain "$FAMILY" "$TABLE" "$CHAIN" >"$TMP_RAW" || return 1

    awk '
    {
        for (i = 1; i < NF; i++) {
            if ($i == "jump" && $(i+1) ~ /^nv_[A-Za-z0-9_]+_dnat$/) {
                target = $(i+1)
                for (j = i+2; j < NF; j++) {
                    if ($j == "handle") {
                        print $(j+1), target
                        break
                    }
                }
            }
        }
    }' "$TMP_RAW"
}

if ! list_rules >"$TMP_RULES"; then
    echo "Error: cannot read nftables rules." >&2
    exit 1
fi

COUNT=0

while IFS=' ' read -r handle target; do
    [ -n "${handle:-}" ] || continue

    prefix=${target#nv_}
    prefix=${prefix%%_*}

    case "$prefix" in
        ????????) ;;
        *) continue ;;
    esac

    if grep -Fqx "$prefix" "$TMP_IDS"; then
        continue
    fi

    COUNT=$((COUNT + 1))
    printf 'Stale rule candidate: handle %s -> %s\n' "$handle" "$target"

    if [ "$MODE" = apply ]; then
        # Re-read handles immediately before deletion.
        if ! list_rules >"$TMP_LIVE"; then
            echo "Error: cannot re-read rules; stopping." >&2
            exit 1
        fi

        if awk -v h="$handle" -v t="$target" \
            '$1 == h && $2 == t { found=1 } END { exit !found }' "$TMP_LIVE"
        then
            nft delete rule "$FAMILY" "$TABLE" "$CHAIN" handle "$handle"
            echo "  Deleted handle $handle"
        else
            echo "  Skipped handle $handle (rule changed or disappeared)"
        fi
    fi
done <"$TMP_RULES"

if [ "$COUNT" -eq 0 ]; then
    echo "No stale Netavark host-port rules found."
elif [ "$MODE" = dry-run ]; then
    echo "Found $COUNT stale rule(s). No changes made."
    echo "Run with --apply to delete them."
else
    echo "Processed $COUNT stale rule candidate(s)."
fi
