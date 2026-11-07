#!/usr/bin/env bash
# Cutover bridge TypeScript → Go (docs/go-rewrite/architecture.md §4, cutover.md §B).
#
# The first update of every existing box into the Go agent is run by the OLD TypeScript agent's
# hard-coded SelfUpdate (spec A §8.3):
#   cd /opt/route-agent && git fetch --all --tags && git reset --hard @{u} && git clean -fd \
#     && npm ci && npm run build && systemctl restart route-agent
# with the unit running `node /opt/route-agent/dist/index.js`. In the cutover commit
# package.json keeps its dependencies and says "build": "bash scripts/bridge-to-go.sh", so this
# script runs between `npm ci` and the restart. It:
#   1. downloads route-agent-linux-<arch> and checksums.txt of the latest release, checks the
#      SHA-256 and that `--version` prints vX.Y.Z[-N-gHASH];
#   2. installs the binary as /opt/route-agent/bin/route-agent (previous one → route-agent.prev);
#   3. keeps the TypeScript unit as route-agent.service.ts-backup and writes the Go unit;
#   4. systemctl daemon-reload;
#   5. schedules a guard (systemd-run, 120 s): if the Go agent is not active by then or keeps
#      restarting, the guard puts the TypeScript unit back, returns the checkout to the commit it
#      had before the update (so the TS agent reports its old version and the orchestrator sees
#      the switch failed) and restarts the TS agent.
# Any failure in 1–4 exits non-zero with the unit as it was, so `&&` stops the chain before
# `systemctl restart` and the old agent keeps running: its dist/ is gitignored (survives
# `git clean -fd`), `npm run build` no longer runs tsup (which wiped dist/), and `npm ci` restored
# the same node_modules the old dist/ needs.
#
# Idempotent: a second run re-verifies, leaves an identical binary and unit as they are, and never
# overwrites the TS backup with the Go unit.
#
# Manual rollback on a box (what the guard does):
#   cp /etc/systemd/system/route-agent.service.ts-backup /etc/systemd/system/route-agent.service
#   git -C /opt/route-agent reset --hard "$(cat /opt/route-agent/bin/ts-rollback-commit)"
#   systemctl daemon-reload && systemctl restart route-agent
#
# Testing only (scripts/test-delivery.sh): ROUTE_AGENT_ROOT prefixes every filesystem path,
# ROUTE_AGENT_RELEASE_BASE replaces the download base URL, ROUTE_AGENT_GUARD_DELAY the guard delay.
#
# The helpers repeat install-go.sh on purpose (see the note there): that file is fetched alone
# with `curl | bash`, and this one is deleted once the fleet is on Go.
set -euo pipefail

ROOT="${ROUTE_AGENT_ROOT:-}"
RELEASE_BASE="${ROUTE_AGENT_RELEASE_BASE:-https://github.com/Ametas/route-agent/releases/latest/download}"
RELEASE_BASE="${RELEASE_BASE%/}"
GUARD_DELAY="${ROUTE_AGENT_GUARD_DELAY:-120}"
GUARD_UNIT="route-agent-bridge-guard"
GUARD_MAX_RESTARTS=3

AGENT_DIR="/opt/route-agent"
BIN_PATH="$AGENT_DIR/bin/route-agent"
ENV_PATH="$AGENT_DIR/.env"
UNIT="route-agent"
UNIT_PATH="/etc/systemd/system/route-agent.service"
UNIT_BACKUP="$UNIT_PATH.ts-backup"
SUDOERS_RULE="/etc/sudoers.d/route-agent-ufw"
TS_COMMIT_FILE="$AGENT_DIR/bin/ts-rollback-commit"
VERSION_RE='^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+-g[0-9a-f]+)?$'

r() { printf '%s%s' "$ROOT" "$1"; }
say() { printf 'bridge-to-go: %s\n' "$*"; }
warn() { printf 'bridge-to-go: WARNING: %s\n' "$*" >&2; }
die() { printf 'bridge-to-go: ERROR: %s — the TypeScript agent keeps running, unit untouched\n' "$*" >&2; exit 1; }

fetch() { curl -fsSL --connect-timeout 10 --max-time 180 --retry 2 --retry-delay 3 "$@"; }

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
}

TEMP_FILES=()
cleanup() { if [[ ${#TEMP_FILES[@]} -gt 0 ]]; then rm -f "${TEMP_FILES[@]}"; fi; }
trap cleanup EXIT

is_go_unit() { [[ -f "$1" ]] && grep -qx "ExecStart=$BIN_PATH" "$1"; }
is_ts_unit() { [[ -f "$1" ]] && grep -q 'dist/index\.js' "$1"; }

go_unit() {
  cat <<EOT
[Unit]
Description=Route Egress gRPC Agent Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$AGENT_DIR
ExecStart=$BIN_PATH
Restart=always
RestartSec=5
EnvironmentFile=$ENV_PATH

[Install]
WantedBy=multi-user.target
EOT
}

# --- guard: runs ~GUARD_DELAY s after the bridge, as a transient systemd unit ------------------

guard() {
  local unit backup state restarts
  unit="$(r "$UNIT_PATH")"
  backup="$(r "$UNIT_BACKUP")"
  if ! is_go_unit "$unit"; then
    say "guard: route-agent.service is not the Go unit any more — nothing to do"
    return 0
  fi
  if [[ ! -f "$backup" ]]; then
    say "guard: no $UNIT_BACKUP — nothing to roll back to"
    return 0
  fi
  state="$(systemctl show "$UNIT" -p ActiveState --value 2>/dev/null || true)"
  restarts="$(systemctl show "$UNIT" -p NRestarts --value 2>/dev/null || true)"
  [[ "$restarts" =~ ^[0-9]+$ ]] || restarts=0
  if [[ "$state" == "active" && "$restarts" -lt "$GUARD_MAX_RESTARTS" ]]; then
    say "guard: Go agent is active (restarts: $restarts) — keeping it"
    return 0
  fi
  warn "guard: Go agent is '$state' after ${restarts} restarts — restoring the TypeScript unit"
  local tmp
  tmp="$(mktemp "$(dirname "$unit")/.route-agent.service.XXXXXX")"
  TEMP_FILES+=("$tmp")
  cp -p "$backup" "$tmp"
  mv -f "$tmp" "$unit"
  systemctl daemon-reload || true
  # The TS agent reports `git describe --tags` of the checkout, which now sits on the release tag
  # of the cutover: restarted as is, it would claim the new version while running the old code,
  # and the orchestrator would count the box as switched. Put the checkout back on the commit it
  # had before the update; dist/ and node_modules/ are ignored and stay as they are. This deletes
  # this very script from the working tree; bash keeps reading it from the open file.
  local commit_file commit
  commit_file="$(r "$TS_COMMIT_FILE")"
  if [[ -f "$commit_file" ]] && command -v git >/dev/null 2>&1; then
    commit="$(head -n 1 "$commit_file")"
    if [[ "$commit" =~ ^[0-9a-f]{40,64}$ ]] && git -C "$(r "$AGENT_DIR")" reset -q --hard "$commit"; then
      say "guard: checkout back at $commit (the version the TS agent reported before)"
    else
      warn "guard: could not put the checkout back at '$commit'; the TS agent will report the cutover version"
    fi
  fi
  systemctl restart "$UNIT"
  say "guard: TypeScript agent restarted from $UNIT_BACKUP"
}

if [[ "${1:-}" == "--guard" ]]; then
  guard
  exit 0
fi
[[ $# -eq 0 ]] || die "unknown argument: $1"

# --- bridge --------------------------------------------------------------------------------------

for tool in curl sha256sum systemctl timeout awk cmp; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
done

unit="$(r "$UNIT_PATH")"
[[ -f "$unit" ]] || die "$UNIT_PATH not found"
[[ -f "$(r "$ENV_PATH")" ]] || die "$ENV_PATH not found"

# 1. download and verify, next to the installed binary (same filesystem → atomic rename)
arch="$(detect_arch)"
asset="route-agent-linux-$arch"
bin="$(r "$BIN_PATH")"
mkdir -p "$(dirname "$bin")"
sums="$(mktemp "$(dirname "$bin")/.checksums.XXXXXX")"
staged="$(mktemp "$(dirname "$bin")/.route-agent.new.XXXXXX")"
TEMP_FILES+=("$sums" "$staged")

say "downloading $RELEASE_BASE/checksums.txt"
fetch "$RELEASE_BASE/checksums.txt" -o "$sums" || die "cannot download checksums.txt from $RELEASE_BASE"
expected="$(awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n) } NF == 2 && n == a { print tolower($1); exit }' "$sums")"
[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "checksums.txt has no valid entry for $asset"

say "downloading $RELEASE_BASE/$asset"
fetch "$RELEASE_BASE/$asset" -o "$staged" || die "cannot download $asset from $RELEASE_BASE"
actual="$(sha256sum "$staged" | awk '{ print $1 }')"
[[ "$actual" == "$expected" ]] || die "checksum mismatch for $asset: expected $expected, got $actual"

chmod 0755 "$staged"
# From / so that a binary built without -ldflags cannot borrow `git describe` of /opt/route-agent.
version="$(cd / && timeout 15 "$staged" --version 2>/dev/null | head -n 1)" || version=""
[[ "$version" =~ $VERSION_RE ]] || die "downloaded binary reports version '$version', expected vX.Y.Z[-N-gHASH]"
say "verified $asset $version"

# 2. install the binary (the TS agent does not use bin/, so this changes nothing it runs)
if [[ -f "$bin" ]] && cmp -s "$staged" "$bin"; then
  say "$BIN_PATH is already $version"
else
  if [[ -f "$bin" ]]; then
    prev="$(mktemp "$(dirname "$bin")/.route-agent.prev.XXXXXX")"
    TEMP_FILES+=("$prev")
    cp -p "$bin" "$prev"
    mv -f "$prev" "$bin.prev"
  fi
  mv -f "$staged" "$bin"
  say "installed $BIN_PATH ($version)"
fi

# 3. keep the TS unit, write the Go unit
if is_ts_unit "$unit"; then
  cp -p "$unit" "$(r "$UNIT_BACKUP")"
  say "TypeScript unit kept as $UNIT_BACKUP"
  # For the guard: the commit the checkout had before this update (`git reset --hard @{u}` of the
  # update chain left it in ORIG_HEAD).
  if command -v git >/dev/null 2>&1 \
    && prev_commit="$(git -C "$(r "$AGENT_DIR")" rev-parse -q --verify 'ORIG_HEAD^{commit}' 2>/dev/null)"; then
    printf '%s\n' "$prev_commit" > "$(r "$TS_COMMIT_FILE")"
  else
    rm -f "$(r "$TS_COMMIT_FILE")"
  fi
fi
old_unit="$(mktemp "$(dirname "$unit")/.route-agent.service.old.XXXXXX")"
new_unit="$(mktemp "$(dirname "$unit")/.route-agent.service.new.XXXXXX")"
TEMP_FILES+=("$old_unit" "$new_unit")
cp -p "$unit" "$old_unit"
go_unit > "$new_unit"
chmod 0644 "$new_unit"
unit_changed=0
if ! cmp -s "$new_unit" "$unit"; then
  mv -f "$new_unit" "$unit"
  unit_changed=1
  say "route-agent.service now runs $BIN_PATH"
fi

# 4. daemon-reload; on failure the previous unit goes back before exiting
if ! systemctl daemon-reload; then
  if [[ "$unit_changed" -eq 1 ]]; then
    mv -f "$old_unit" "$unit"
    systemctl daemon-reload || true
  fi
  die "systemctl daemon-reload failed"
fi

# Not part of the switch (never fails the bridge): drop the world-usable ufw sudoers rule
# (architecture §6 item 2) if root's sudo works without it. Both agents run as root.
rule="$(r "$SUDOERS_RULE")"
if [[ -f "$rule" ]] && command -v sudo >/dev/null 2>&1; then
  mv -f "$rule" "$rule.removed-by-route-agent"
  if sudo -n true >/dev/null 2>&1; then
    rm -f "$rule.removed-by-route-agent"
    say "removed $SUDOERS_RULE"
  else
    mv -f "$rule.removed-by-route-agent" "$rule"
    warn "root cannot run sudo without $SUDOERS_RULE; left it in place"
  fi
fi

# 5. guard. A separate transient unit: it must outlive route-agent's control group, which the
# chain's `systemctl restart` is about to kill. Best effort: without it the box still switches.
if [[ -f "$(r "$UNIT_BACKUP")" ]]; then
  self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
  guard_env=()
  [[ -n "$ROOT" ]] && guard_env+=(--setenv=ROUTE_AGENT_ROOT="$ROOT")
  systemctl stop "$GUARD_UNIT.timer" >/dev/null 2>&1 || true
  systemctl reset-failed "$GUARD_UNIT.service" "$GUARD_UNIT.timer" >/dev/null 2>&1 || true
  if command -v systemd-run >/dev/null 2>&1 \
    && systemd-run --unit="$GUARD_UNIT" --on-active="$GUARD_DELAY" --timer-property=AccuracySec=1s \
      --collect ${guard_env[@]+"${guard_env[@]}"} /bin/bash "$self" --guard >/dev/null 2>&1; then
    say "guard scheduled in ${GUARD_DELAY}s ($GUARD_UNIT): rolls back to the TS unit if the Go agent is not up"
  else
    warn "could not schedule the rollback guard; if the Go agent fails to start, roll back by hand (see the header of this script)"
  fi
fi

say "done: the update chain's 'systemctl restart route-agent' starts the Go agent $version"
