#!/usr/bin/env bash
# Installer of the Go route-agent (docs/go-rewrite/cutover.md). install.sh stays the TypeScript
# installer until the cutover commit (branch alpha merged into main), which replaces it with this
# file.
#
# Run it with bash explicitly (`curl … | bash -s -- …` or `bash install-go.sh …`): the repository
# is edited on Windows, the exec bit does not survive.
#
# Modes:
#   fresh (default)  same CLI as install.sh: --secret <bundle|secret> [--port 8081] [--repo URL]
#                    [--domain D] [--decoy-port 8443]. Writes .env, certificates, the binary and
#                    the unit, then starts the agent.
#   --convert        an existing TypeScript box: keeps /opt/route-agent/.env and the certificates
#                    untouched, installs the binary, swaps the unit (the TS one is kept as
#                    route-agent.service.ts-backup), restarts and checks the agent within 30 s;
#                    on failure restores the TS unit and starts the TS agent again.
#   --rollback       puts route-agent.service.ts-backup back and starts the TS agent.
#   --channel stable|nightly
#                    stable  → <repo>/releases/latest/download (the latest real release)
#                    nightly → <repo>/releases/download/nightly (prerelease built from branch alpha)
#                    The channel is written to /opt/route-agent/bin/channel: the agent's SelfUpdate
#                    keeps updating from it (docs/go-rewrite/update.md).
#
# What a box gets (spec A §12.3, minus the Node/npm/git parts the Go agent does not need):
#   /opt/route-agent/.env (0600), /etc/route-agent/certs/{ca.crt,agent.crt,agent.key(0600)},
#   /opt/route-agent/bin/route-agent (SHA-256 against checksums.txt, `--version` shape checked
#   before it is installed; the previous binary is kept as route-agent.prev),
#   /opt/route-agent/bin/channel (0644, `stable` or `nightly`),
#   /etc/systemd/system/route-agent.service with ExecStart=/opt/route-agent/bin/route-agent.
#
# No sudoers rule: the Go agent runs as root and calls ufw directly, without sudo. The old
# `ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw` rule gave ufw-as-root to every user of the box
# (architecture §6 item 2); it is removed.
#
# Testing only (scripts/test-delivery.sh): ROUTE_AGENT_ROOT prefixes every filesystem path,
# ROUTE_AGENT_RELEASE_BASE replaces the download base URL.
#
# The download/verify/unit helpers are repeated in scripts/bridge-to-go.sh on purpose: this file
# is fetched alone (`curl | bash`) and cannot source a sibling, and the bridge is a one-shot
# transitional script that goes away once the fleet is on Go. scripts/test-delivery.sh runs the
# same failure cases against both.
set -euo pipefail

# Default channel. On branch alpha it is `nightly`: before the cutover releases/latest has no Go
# assets, and the orchestrator's install wrapper cannot pass --channel. The cutover commit
# switches it to `stable` (docs/go-rewrite/cutover.md §B).
DEFAULT_CHANNEL="nightly"

ROOT="${ROUTE_AGENT_ROOT:-}"
AGENT_DIR="/opt/route-agent"
BIN_PATH="$AGENT_DIR/bin/route-agent"
ENV_PATH="$AGENT_DIR/.env"
CERTS_DIR="/etc/route-agent/certs"
UNIT="route-agent"
UNIT_PATH="/etc/systemd/system/route-agent.service"
UNIT_BACKUP="$UNIT_PATH.ts-backup"
SUDOERS_RULE="/etc/sudoers.d/route-agent-ufw"
# The orchestrator's parser (orch:src/core/services/agentCatchUp.ts), spec A §7.3.
VERSION_RE='^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+-g[0-9a-f]+)?$'
HEALTH_TIMEOUT=30
HEALTH_STABLE=5

r() { printf '%s%s' "$ROOT" "$1"; }
say() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

fetch() { curl -fsSL --connect-timeout 10 --max-time 180 --retry 2 --retry-delay 3 "$@"; }
reachable() { curl -fsS -o /dev/null --connect-timeout 8 --max-time 15 "$1"; }

# --- arguments ---------------------------------------------------------------------------------

SECRET=""
PORT="8081"
REPO="https://github.com/Ametas/route-agent.git"
DOMAIN=""
DECOY_PORT="8443"
CHANNEL="$DEFAULT_CHANNEL"
MODE="fresh"

need_value() { [[ $# -ge 2 && -n "$2" ]] || die "$1 needs a value"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --secret) need_value "$@"; SECRET="$2"; shift ;;
    --port) need_value "$@"; PORT="$2"; shift ;;
    --repo) need_value "$@"; REPO="$2"; shift ;;
    --domain) need_value "$@"; DOMAIN="$2"; shift ;;
    --decoy-port) need_value "$@"; DECOY_PORT="$2"; shift ;;
    --channel) need_value "$@"; CHANNEL="$2"; shift ;;
    --convert) MODE="convert" ;;
    --rollback) MODE="rollback" ;;
    *) echo "Unknown parameter: $1"; exit 1 ;;
  esac
  shift
done

case "$CHANNEL" in
  stable|nightly) ;;
  *) die "--channel must be stable or nightly, got '$CHANNEL'" ;;
esac
[[ "$PORT" =~ ^[0-9]+$ ]] || die "--port must be a number, got '$PORT'"

# Release assets come from the GitHub repository --repo points at (install.sh cloned it).
release_base() {
  if [[ -n "${ROUTE_AGENT_RELEASE_BASE:-}" ]]; then
    printf '%s' "${ROUTE_AGENT_RELEASE_BASE%/}"
    return
  fi
  local repo="${REPO%/}"
  repo="${repo%.git}"
  [[ "$repo" =~ ^https://github\.com/[^/]+/[^/]+$ ]] \
    || die "--repo must be a https://github.com/<owner>/<repo> URL (assets come from its releases), got '$REPO'"
  if [[ "$CHANNEL" == "stable" ]]; then
    printf '%s/releases/latest/download' "$repo"
  else
    printf '%s/releases/download/nightly' "$repo"
  fi
}

# --- binary: download, verify, install ---------------------------------------------------------

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
}

STAGE_FILES=()
cleanup_stage() { if [[ ${#STAGE_FILES[@]} -gt 0 ]]; then rm -f "${STAGE_FILES[@]}"; fi; }
trap cleanup_stage EXIT

# stage_binary <base-url>: downloads the asset for this machine next to the installed binary
# (same filesystem, so the final rename is atomic), checks it against checksums.txt and runs
# `--version`. Sets STAGED_PATH and STAGED_VERSION. Touches nothing that is in use.
stage_binary() {
  local base="$1" arch asset dir sums tmp expected actual ver
  arch="$(detect_arch)"
  asset="route-agent-linux-$arch"
  dir="$(dirname "$(r "$BIN_PATH")")"
  mkdir -p "$dir"
  sums="$(mktemp "$dir/.checksums.XXXXXX")"
  tmp="$(mktemp "$dir/.route-agent.new.XXXXXX")"
  STAGE_FILES+=("$sums" "$tmp")

  say "Downloading $base/checksums.txt"
  fetch "$base/checksums.txt" -o "$sums" || die "cannot download checksums.txt from $base"
  expected="$(awk -v a="$asset" '{ n = $2; sub(/^\*/, "", n) } NF == 2 && n == a { print tolower($1); exit }' "$sums")"
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "checksums.txt has no valid entry for $asset"

  say "Downloading $base/$asset"
  fetch "$base/$asset" -o "$tmp" || die "cannot download $asset from $base"
  actual="$(sha256sum "$tmp" | awk '{ print $1 }')"
  [[ "$actual" == "$expected" ]] || die "checksum mismatch for $asset: expected $expected, got $actual"

  chmod 0755 "$tmp"
  # From / so that a binary built without -ldflags cannot borrow `git describe` of a checkout.
  ver="$(cd / && timeout 15 "$tmp" --version 2>/dev/null | head -n 1)" || ver=""
  [[ "$ver" =~ $VERSION_RE ]] || die "downloaded binary reports version '$ver', expected vX.Y.Z[-N-gHASH]"

  STAGED_PATH="$tmp"
  STAGED_VERSION="$ver"
  say "Verified $asset $ver (sha256 $actual)"
}

# install_staged: the staged binary replaces the installed one by rename; the installed one is
# kept as route-agent.prev for a manual rollback (same convention as the Go SelfUpdate).
install_staged() {
  local dest prev
  dest="$(r "$BIN_PATH")"
  if [[ -f "$dest" ]] && cmp -s "$STAGED_PATH" "$dest"; then
    say "Binary $BIN_PATH is already $STAGED_VERSION"
    return
  fi
  if [[ -f "$dest" ]]; then
    prev="$(mktemp "$(dirname "$dest")/.route-agent.prev.XXXXXX")"
    STAGE_FILES+=("$prev")
    cp -p "$dest" "$prev"
    mv -f "$prev" "$dest.prev"
  fi
  mv -f "$STAGED_PATH" "$dest"
  say "Installed $BIN_PATH ($STAGED_VERSION)"
}

# write_channel: the channel of the installed binary, next to it; SelfUpdate reads it when the
# orchestrator does not name a channel (internal/update, ChannelFileName).
write_channel() {
  printf '%s\n' "$CHANNEL" | write_file "$(dirname "$(r "$BIN_PATH")")/channel" 0644
}

# --- unit --------------------------------------------------------------------------------------

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

is_ts_unit() { [[ -f "$1" ]] && grep -q 'dist/index\.js' "$1"; }

# write_file <path> <mode>: stdin → temp file in the same directory → rename. Unchanged content
# is not rewritten.
write_file() {
  local dest="$1" mode="$2" tmp
  mkdir -p "$(dirname "$dest")"
  tmp="$(mktemp "$(dirname "$dest")/.$(basename "$dest").XXXXXX")"
  STAGE_FILES+=("$tmp")
  cat > "$tmp"
  chmod "$mode" "$tmp"
  if [[ -f "$dest" ]] && cmp -s "$tmp" "$dest"; then
    rm -f "$tmp"
    return
  fi
  mv -f "$tmp" "$dest"
}

backup_ts_unit() {
  local unit
  unit="$(r "$UNIT_PATH")"
  if is_ts_unit "$unit"; then
    cp -p "$unit" "$(r "$UNIT_BACKUP")"
    say "TypeScript unit kept as $UNIT_BACKUP"
  fi
}

# --- sudoers -----------------------------------------------------------------------------------

remove_insecure_sudoers() {
  local rule
  rule="$(r "$SUDOERS_RULE")"
  [[ -f "$rule" ]] || return 0
  # The Go agent runs as root and calls ufw directly (no sudo), so the rule only widened ufw to
  # every user of the box (architecture §6 item 2).
  rm -f "$rule"
  say "Removed $SUDOERS_RULE (the Go agent runs ufw as root, without sudo)"
}

# --- health ------------------------------------------------------------------------------------

port_listening() {
  command -v ss >/dev/null 2>&1 || return 0
  ss -ltn 2>/dev/null | awk -v p=":$1" '$4 ~ p "$" { found = 1 } END { exit !found }'
}

# wait_healthy <port>: the unit is active and the port is listening for HEALTH_STABLE consecutive
# seconds within HEALTH_TIMEOUT. A crash loop (Restart=always, RestartSec=5) shows up as a
# non-active state between attempts, so "active once" is not enough.
wait_healthy() {
  local port="$1" stable=0 i state
  for ((i = 0; i < HEALTH_TIMEOUT; i++)); do
    state="$(systemctl is-active "$UNIT" 2>/dev/null || true)"
    if [[ "$state" == "active" ]] && port_listening "$port"; then
      stable=$((stable + 1))
      if ((stable >= HEALTH_STABLE)); then
        return 0
      fi
    else
      stable=0
    fi
    sleep 1
  done
  return 1
}

journal_tail() {
  if command -v journalctl >/dev/null 2>&1; then
    journalctl -u "$UNIT" -n 30 --no-pager 2>/dev/null || true
  fi
}

env_port() {
  local p=""
  [[ -f "$(r "$ENV_PATH")" ]] && p="$(sed -n 's/^PORT=//p' "$(r "$ENV_PATH")" | tail -n 1 | tr -d '[:space:]"')"
  [[ "$p" =~ ^[0-9]+$ ]] || p=""
  printf '%s' "${p:-8081}"
}

# restore_ts_unit: back to the TypeScript agent from route-agent.service.ts-backup. Its dist/ is
# never touched by this script.
restore_ts_unit() {
  local port="$1" backup
  backup="$(r "$UNIT_BACKUP")"
  [[ -f "$backup" ]] || die "no $UNIT_BACKUP to roll back to"
  systemctl stop "$UNIT" || true
  write_file "$(r "$UNIT_PATH")" 0644 < "$backup"
  systemctl daemon-reload
  systemctl start "$UNIT" || true
  if wait_healthy "$port"; then
    say "TypeScript agent restored from $UNIT_BACKUP and running."
    return 0
  fi
  warn "TypeScript unit restored, but the agent is not healthy — check: journalctl -u $UNIT"
  return 1
}

# --- modes -------------------------------------------------------------------------------------

require_root() {
  if [[ -z "$ROOT" && "${EUID}" -ne 0 ]]; then
    die "please run as root"
  fi
}

require_tools() {
  local t
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 || die "required tool not found: $t"
  done
}

do_rollback() {
  require_root
  require_tools systemctl
  restore_ts_unit "$(env_port)" || exit 1
}

do_convert() {
  require_root
  require_tools curl sha256sum systemctl timeout
  local unit port
  unit="$(r "$UNIT_PATH")"
  [[ -f "$(r "$ENV_PATH")" ]] || die "$ENV_PATH not found — this is not an installed box; run without --convert"
  [[ -f "$unit" ]] || die "$UNIT_PATH not found — run without --convert"
  [[ -z "$SECRET" ]] || warn "--secret is ignored with --convert: the existing .env and certificates stay as they are"
  port="$(env_port)"

  say "Converting this box to the Go agent (channel: $CHANNEL)."
  stage_binary "$(release_base)"   # any failure here: nothing on the box has changed
  install_staged                   # the TS agent does not use bin/
  write_channel
  backup_ts_unit
  if ! is_ts_unit "$unit" && [[ ! -f "$(r "$UNIT_BACKUP")" ]]; then
    warn "the current unit is not the TypeScript one and there is no $UNIT_BACKUP: no automatic rollback"
  fi
  remove_insecure_sudoers

  say "Switching route-agent.service to $BIN_PATH"
  systemctl stop "$UNIT" || true
  if go_unit | write_file "$unit" 0644 \
    && systemctl daemon-reload \
    && systemctl enable "$UNIT" >/dev/null 2>&1 \
    && systemctl start "$UNIT" \
    && wait_healthy "$port"; then
    say "Go agent $STAGED_VERSION is running on port $port."
    say "Rollback: bash install-go.sh --rollback (restores $UNIT_BACKUP)."
    return 0
  fi

  warn "the Go agent did not come up within ${HEALTH_TIMEOUT}s. Its log:"
  journal_tail >&2
  if [[ -f "$(r "$UNIT_BACKUP")" ]]; then
    warn "rolling back to the TypeScript agent"
    restore_ts_unit "$port" || true
  fi
  die "conversion failed; the TypeScript agent's dist/ was not touched"
}

# Certificates and secret from the orchestrator's bundle: base64(JSON {ca, cert, key, secret})
# (orch:src/shared/utils/certs.ts). Anything else is a raw secret (legacy path, no certificates).
# The secret goes through stdin, never through argv.
unpack_secret() {
  local json certs
  # tr: a raw hex secret is valid base64 and decodes to binary; NULs would make bash warn.
  if json="$(printf '%s' "$SECRET" | base64 -d 2>/dev/null | tr -d '\000')" \
    && printf '%s' "$json" | jq -e '(.secret|type=="string") and (.ca|type=="string") and (.cert|type=="string") and (.key|type=="string")' >/dev/null 2>&1; then
    certs="$(r "$CERTS_DIR")"
    mkdir -p "$certs"
    printf '%s' "$json" | jq -j '.ca' | write_file "$certs/ca.crt" 0644
    printf '%s' "$json" | jq -j '.cert' | write_file "$certs/agent.crt" 0644
    (umask 077 && printf '%s' "$json" | jq -j '.key' | write_file "$certs/agent.key" 0600)
    SECRET="$(printf '%s' "$json" | jq -j '.secret')"
    say "Unpacked mTLS certificates and secret into $CERTS_DIR/"
  fi
}

install_packages() {
  export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 NEEDRESTART_MODE=l
  say "Installing system packages..."
  apt-get update
  # As install.sh minus git (no checkout) plus jq (bundle).
  apt-get install -y iptables iproute2 ufw curl unzip xz-utils mtr-tiny conntrack jq \
    debian-keyring debian-archive-keyring apt-transport-https ca-certificates gnupg

  if ! command -v caddy >/dev/null 2>&1; then
    local caddy_repo="https://dl.cloudsmith.io/public/caddy/stable" tmp
    if reachable "$caddy_repo/gpg.key"; then
      say "Adding Caddy official apt repository..."
      tmp="$(mktemp)"
      fetch "$caddy_repo/gpg.key" -o "$tmp"
      gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg "$tmp"
      rm -f "$tmp"
      fetch "$caddy_repo/debian.deb.txt" -o /etc/apt/sources.list.d/caddy-stable.list
      apt-get update
    else
      # The distro Caddy is enough: the orchestrator pushes its own build anyway.
      warn "Cloudsmith is unreachable — installing Caddy from the distro repository."
      rm -f /etc/apt/sources.list.d/caddy-stable.list
    fi
    apt-get install -y caddy
  fi
}

do_fresh() {
  require_root
  [[ -n "$SECRET" ]] || die "--secret parameter is required."
  say "Installing the Go Route Agent (channel: $CHANNEL)..."

  install_packages
  require_tools curl sha256sum systemctl timeout jq base64

  ufw allow 22/tcp || true
  ufw allow 443/tcp || true
  ufw allow 443/udp || true
  ufw allow "$PORT"/tcp || true
  ufw --force enable || true
  remove_insecure_sudoers

  # Base layout (spec A §12.2 step 8). Unlike install.sh, an existing sing-box config is kept: a
  # re-run on a working box must not wipe what the orchestrator pushed (risk R-7).
  mkdir -p "$(r /etc/sing-box)" "$(r /var/www/decoy)" "$(r /var/lib/caddy)" "$(r /etc/amnezia/amneziawg)" "$(r "$AGENT_DIR")"
  chmod 755 "$(r /var/lib/caddy)" || true
  if [[ ! -f "$(r /etc/sing-box/config.json)" ]]; then
    echo '{"route":{"rules":[]}}' > "$(r /etc/sing-box/config.json)"
  fi

  # Download before writing anything of the agent's own: a failed download leaves no half-box.
  stage_binary "$(release_base)"

  unpack_secret
  (umask 077 && write_file "$(r "$ENV_PATH")" 0600 <<EOT
PORT=$PORT
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=$SECRET
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
CA_CERT_PATH=/etc/route-agent/certs/ca.crt
AGENT_CERT_PATH=/etc/route-agent/certs/agent.crt
AGENT_KEY_PATH=/etc/route-agent/certs/agent.key
EOT
  )

  install_staged
  write_channel
  backup_ts_unit
  go_unit | write_file "$(r "$UNIT_PATH")" 0644
  systemctl daemon-reload
  systemctl enable "$UNIT"
  systemctl restart "$UNIT"

  if ! wait_healthy "$PORT"; then
    warn "the agent did not come up within ${HEALTH_TIMEOUT}s. Its log:"
    journal_tail >&2
    die "installation finished, but route-agent is not running"
  fi
  if [[ -n "$DOMAIN" ]]; then
    say "Route Agent $STAGED_VERSION initialized successfully on port $PORT (domain: $DOMAIN, decoy port: $DECOY_PORT)! Awaiting Orchestrator binary push..."
  else
    say "Route Agent $STAGED_VERSION initialized successfully on port $PORT! Awaiting Orchestrator binary push..."
  fi
}

case "$MODE" in
  fresh) do_fresh ;;
  convert) do_convert ;;
  rollback) do_rollback ;;
esac
