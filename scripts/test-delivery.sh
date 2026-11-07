#!/usr/bin/env bash
# Dry runs of the delivery scripts against a fake release served locally.
#
#   bash scripts/test-delivery.sh
#
# Needs go, python3, curl; shellcheck if installed (skipped with a note otherwise). Runs as any
# user: everything happens in a temporary root (ROUTE_AGENT_ROOT) with fake systemctl, ss, sudo,
# systemd-run, apt-get, ufw, caddy, journalctl and sleep on PATH that record their calls.
#
# The release is real: binaries built by scripts/build-release.sh and a real checksums.txt,
# served by `python3 -m http.server` (ROUTE_AGENT_RELEASE_BASE). Variants: good, checksum
# mismatch, binary with a bad --version, missing asset, missing release. The default release URLs
# of each channel (no ROUTE_AGENT_RELEASE_BASE) are checked with a fake curl that only records.
#
# SC2016 (file-wide): the single-quoted bodies of the generated fakes and of `bash -c` checks
# expand later, on purpose.
# shellcheck disable=SC2016
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/route-agent-delivery.XXXXXX")"
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]]; then kill "$SERVER_PID" 2>/dev/null || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; }
check() { # check <description> <command...>
  local d="$1"; shift
  if "$@"; then ok "$d"; else bad "$d"; fi
}
section() { printf '\n== %s\n' "$1"; }

SCRIPTS=(install-go.sh scripts/bridge-to-go.sh scripts/build-release.sh scripts/test-delivery.sh)

# --- static checks -------------------------------------------------------------------------------
section "static checks"
for s in "${SCRIPTS[@]}"; do
  check "bash -n $s" bash -n "$REPO/$s"
done
if command -v shellcheck >/dev/null 2>&1; then
  for s in "${SCRIPTS[@]}"; do
    check "shellcheck $s" shellcheck "$REPO/$s"
  done
else
  printf '  SKIP  shellcheck not installed\n'
fi

# --- fake release ----------------------------------------------------------------------------------
section "building the fake release"
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH=amd64 ;;
  aarch64|arm64) HOST_ARCH=arm64 ;;
  *) echo "unsupported host architecture $(uname -m)"; exit 1 ;;
esac
ASSET="route-agent-linux-$HOST_ARCH"
GOOD_VERSION="v9.9.9-3-gabc1234"
REL="$WORK/release"

bash "$REPO/scripts/build-release.sh" "$GOOD_VERSION" "$REL/good" >/dev/null
echo "  built $GOOD_VERSION for amd64 and arm64"

mkdir -p "$REL/badsum" "$REL/badver" "$REL/missing"
cp "$REL/good"/route-agent-linux-* "$REL/badsum/"
awk -v a="$ASSET" '$2 == a { $1 = "0000000000000000000000000000000000000000000000000000000000000000" } { print $1 "  " $2 }' \
  "$REL/good/checksums.txt" > "$REL/badsum/checksums.txt"

# A binary whose version is not vX.Y.Z[-N-gHASH] (e.g. built from the moving nightly tag).
(cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH="$HOST_ARCH" go build -trimpath \
  -ldflags "-s -w -X github.com/Ametas/route-agent/internal/version.Version=nightly-1-gdeadbee" \
  -o "$REL/badver/$ASSET" ./cmd/route-agent)
(cd "$REL/badver" && sha256sum "$ASSET" > checksums.txt)

# checksums.txt lists the asset, the asset itself is absent.
cp "$REL/good/checksums.txt" "$REL/missing/"

python3 -I -u -m http.server 0 --bind 127.0.0.1 --directory "$REL" > "$WORK/server.log" 2>&1 &
SERVER_PID=$!
PORT=""
for _ in $(seq 1 50); do
  PORT="$(sed -n 's/.*port \([0-9][0-9]*\).*/\1/p' "$WORK/server.log" | head -n 1)"
  [[ -n "$PORT" ]] && break
  /bin/sleep 0.1
done
[[ -n "$PORT" ]] || { echo "http.server did not start"; cat "$WORK/server.log"; exit 1; }
BASE="http://127.0.0.1:$PORT"
export NO_PROXY="127.0.0.1,localhost${NO_PROXY:+,$NO_PROXY}" no_proxy="127.0.0.1,localhost${no_proxy:+,$no_proxy}"
echo "  serving $REL at $BASE"

# --- fakes -----------------------------------------------------------------------------------------
FAKEBIN="$WORK/fakebin"
mkdir -p "$FAKEBIN"
# healthy(): the TS unit always comes up; the Go unit comes up unless FAKE_GO_HEALTHY=0.
cat > "$FAKEBIN/_healthy" <<'EOF'
unit_file="$ROUTE_AGENT_ROOT/etc/systemd/system/route-agent.service"
healthy() {
  grep -q 'dist/index.js' "$unit_file" 2>/dev/null && return 0
  [[ "${FAKE_GO_HEALTHY:-1}" == 1 ]]
}
EOF
cat > "$FAKEBIN/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "systemctl $*" >> "$FAKE_LOG"
. "$(dirname "$0")/_healthy"
case "$1" in
  is-active) if healthy; then echo active; else echo activating; exit 3; fi ;;
  show)
    case "$*" in
      *ActiveState*) if healthy; then echo active; else echo activating; fi ;;
      *NRestarts*) if healthy; then echo "${FAKE_NRESTARTS:-0}"; else echo 7; fi ;;
    esac ;;
  daemon-reload) exit "${FAKE_DAEMON_RELOAD_RC:-0}" ;;
esac
exit 0
EOF
cat > "$FAKEBIN/ss" <<'EOF'
#!/usr/bin/env bash
. "$(dirname "$0")/_healthy"
port="$(sed -n 's/^PORT=//p' "$ROUTE_AGENT_ROOT/opt/route-agent/.env" | tail -n 1)"
echo "State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process"
if healthy; then echo "LISTEN 0      4096         0.0.0.0:${port:-8081}      0.0.0.0:*"; fi
EOF
for f in sudo systemd-run apt-get ufw caddy gpg; do
  printf '#!/usr/bin/env bash\necho "%s $*" >> "$FAKE_LOG"\nexit 0\n' "$f" > "$FAKEBIN/$f"
done
printf '#!/usr/bin/env bash\necho "(fake journal of $*)"\n' > "$FAKEBIN/journalctl"
printf '#!/usr/bin/env bash\nexit 0\n' > "$FAKEBIN/sleep"
chmod +x "$FAKEBIN"/*
export PATH="$FAKEBIN:$PATH"
# A curl that records its arguments and fails like a 404: put first on PATH to see which URL a
# script asks for without the network.
FAKECURL="$WORK/fakecurl"
mkdir -p "$FAKECURL"
printf '#!/usr/bin/env bash\necho "curl $*" >> "$FAKE_LOG"\nexit 22\n' > "$FAKECURL/curl"
chmod +x "$FAKECURL/curl"
STABLE_URL="https://github.com/Ametas/route-agent/releases/latest/download"
NIGHTLY_URL="https://github.com/Ametas/route-agent/releases/download/nightly"

# --- fixtures --------------------------------------------------------------------------------------
TS_UNIT='[Unit]
Description=Route Egress gRPC Agent Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/route-agent
ExecStart=/usr/bin/node /opt/route-agent/dist/index.js
Restart=always
RestartSec=5
EnvironmentFile=/opt/route-agent/.env

[Install]
WantedBy=multi-user.target'
printf '%s\n' "$TS_UNIT" > "$WORK/ts-unit"

N=0
# new_case <name>: fresh temp root and call log; sets ROOT, LOG, OUT.
new_case() {
  N=$((N + 1))
  ROOT="$WORK/case$N-$1"
  LOG="$ROOT.calls"
  OUT="$ROOT.out"
  mkdir -p "$ROOT"
  : > "$LOG"
  printf '\n-- %s\n' "$1"
}

# A box installed by the TypeScript install.sh (spec A §12.3).
make_ts_box() {
  mkdir -p "$ROOT/etc/systemd/system" "$ROOT/opt/route-agent/dist" "$ROOT/etc/route-agent/certs" "$ROOT/etc/sudoers.d"
  cp "$WORK/ts-unit" "$ROOT/etc/systemd/system/route-agent.service"
  printf 'PORT=8081\nHOST=0.0.0.0\nEGRESS_CONTROL_SECRET=%s\nSINGBOX_CONFIG_PATH=/etc/sing-box/config.json\nRELOAD_COMMAND=systemctl reload sing-box\n' \
    "$(printf 'a%.0s' $(seq 1 64))" > "$ROOT/opt/route-agent/.env"
  chmod 600 "$ROOT/opt/route-agent/.env"
  echo 'console.log("ts agent")' > "$ROOT/opt/route-agent/dist/index.js"
  echo "CA" > "$ROOT/etc/route-agent/certs/ca.crt"
  echo "CERT" > "$ROOT/etc/route-agent/certs/agent.crt"
  echo "KEY" > "$ROOT/etc/route-agent/certs/agent.key"
  chmod 600 "$ROOT/etc/route-agent/certs/agent.key"
  echo 'ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw' > "$ROOT/etc/sudoers.d/route-agent-ufw"
  STATE_BEFORE="$(state_hash)"
}

# Hash of what must survive: .env, certificates, dist/.
state_hash() {
  (cd "$ROOT" && find opt/route-agent/.env opt/route-agent/dist etc/route-agent/certs -type f -exec sha256sum {} + 2>/dev/null | sort; \
    stat -c '%a %n' opt/route-agent/.env etc/route-agent/certs/agent.key 2>/dev/null)
}

# run <script> [args...]: env + capture; sets RC.
run() {
  local script="$1"; shift
  set +e
  ROUTE_AGENT_ROOT="$ROOT" FAKE_LOG="$LOG" bash "$REPO/$script" "$@" > "$OUT" 2>&1
  RC=$?
  set -e
}

UNIT_F() { printf '%s' "$ROOT/etc/systemd/system/route-agent.service"; }
BACKUP_F() { printf '%s' "$ROOT/etc/systemd/system/route-agent.service.ts-backup"; }
BIN_F() { printf '%s' "$ROOT/opt/route-agent/bin/route-agent"; }

rc_zero() { [[ "$RC" -eq 0 ]] || { sed 's/^/        | /' "$OUT"; return 1; }; }
rc_nonzero() { [[ "$RC" -ne 0 ]]; }
unit_is_ts() { cmp -s "$(UNIT_F)" "$WORK/ts-unit"; }
unit_is_go() { grep -qx 'ExecStart=/opt/route-agent/bin/route-agent' "$(UNIT_F)" && grep -qx 'WorkingDirectory=/opt/route-agent' "$(UNIT_F)" && grep -qx 'Restart=always' "$(UNIT_F)" && grep -qx 'RestartSec=5' "$(UNIT_F)" && grep -qx 'EnvironmentFile=/opt/route-agent/.env' "$(UNIT_F)"; }
backup_is_ts() { cmp -s "$(BACKUP_F)" "$WORK/ts-unit"; }
no_backup() { [[ ! -e "$(BACKUP_F)" ]]; }
bin_is_good() { [[ -x "$(BIN_F)" ]] && [[ "$(cd / && "$(BIN_F)" --version)" == "$GOOD_VERSION" ]]; }
no_bin() { [[ ! -e "$(BIN_F)" ]]; }
CHANNEL_F() { printf '%s' "$ROOT/opt/route-agent/bin/channel"; }
# channel_is <stable|nightly>: bin/channel holds exactly that line, mode 644.
channel_is() { [[ "$(cat "$(CHANNEL_F)" 2>/dev/null)" == "$1" && "$(stat -c %a "$(CHANNEL_F)")" == 644 ]]; }
no_channel() { [[ ! -e "$(CHANNEL_F)" ]]; }
called() { grep -qF -- "$1" "$LOG"; }
not_called_x() { ! grep -qxF -- "$1" "$LOG"; }
not_called() { ! grep -qF -- "$1" "$LOG"; }
said() { grep -qF -- "$1" "$OUT"; }
state_kept() { [[ "$(state_hash)" == "$STATE_BEFORE" ]]; }
no_temp_left() { [[ -z "$(find "$ROOT" -name '.*.??????' -o -name '.route-agent.*' -o -name '.checksums.*' | head -n 1)" ]]; }
sudoers_gone() { [[ ! -e "$ROOT/etc/sudoers.d/route-agent-ufw" ]]; }
# order <a> <b>: the first call matching a comes before the first call matching b.
order() {
  local a b
  a="$(grep -nF -- "$1" "$LOG" | head -n 1 | cut -d: -f1)"
  b="$(grep -nF -- "$2" "$LOG" | head -n 1 | cut -d: -f1)"
  [[ -n "$a" && -n "$b" && "$a" -lt "$b" ]]
}

# The download fails at some step: nothing on the box may change and nothing may be reloaded.
expect_untouched() { # expect_untouched <message fragment>
  check "exits non-zero" rc_nonzero
  check "says: $1" said "$1"
  check "unit unchanged" unit_is_ts
  check "no backup written" no_backup
  check "no binary installed" no_bin
  check "no channel file written" no_channel
  check "no daemon-reload / stop / start" not_called "systemctl daemon-reload"
  check "no stop of route-agent" not_called_x "systemctl stop route-agent"
  check ".env, certs, dist/ unchanged" state_kept
  check "no temp files left" no_temp_left
}

# --- bridge-to-go.sh -------------------------------------------------------------------------------
section "scripts/bridge-to-go.sh"

new_case "bridge: success"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh
check "exits 0" rc_zero
check "unit runs /opt/route-agent/bin/route-agent (WorkingDirectory, Restart, EnvironmentFile kept)" unit_is_go
check "TS unit kept as route-agent.service.ts-backup" backup_is_ts
check "binary installed, --version = $GOOD_VERSION" bin_is_good
check "daemon-reload called" called "systemctl daemon-reload"
check "does not stop route-agent itself" not_called_x "systemctl stop route-agent"
check "no 'systemctl restart route-agent'" not_called "systemctl restart route-agent"
check "rollback guard scheduled via systemd-run" called "systemd-run --unit=route-agent-bridge-guard --on-active=120"
check "insecure sudoers rule removed (root sudo works)" sudoers_gone
check "bin/channel = stable (the default), mode 644" channel_is stable
check ".env, certs, dist/ unchanged" state_kept
check "no temp files left" no_temp_left
BRIDGE_ROOT="$ROOT"

new_case "bridge: --channel nightly"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh --channel nightly
check "exits 0" rc_zero
check "unit runs the Go binary" unit_is_go
check "binary installed" bin_is_good
check "bin/channel = nightly, mode 644" channel_is nightly
check "says the channel" said "channel nightly"
check "no temp files left" no_temp_left

new_case "bridge: default release URL per channel (no override)"
make_ts_box
PATH="$FAKECURL:$PATH" run scripts/bridge-to-go.sh --channel nightly
check "nightly fails without the network" rc_nonzero
check "nightly asks $NIGHTLY_URL/checksums.txt" called "$NIGHTLY_URL/checksums.txt"
PATH="$FAKECURL:$PATH" run scripts/bridge-to-go.sh
check "no --channel asks $STABLE_URL/checksums.txt" called "$STABLE_URL/checksums.txt"
check "box untouched" unit_is_ts
check "no channel file" no_channel

new_case "bridge: second run is idempotent"
cp -a "$BRIDGE_ROOT/." "$ROOT/"
STATE_BEFORE="$(state_hash)"
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh
check "exits 0" rc_zero
check "unit still the Go unit" unit_is_go
check "backup still the TS unit (not overwritten with the Go one)" backup_is_ts
check "identical binary not replaced (no route-agent.prev)" test ! -e "$(BIN_F).prev"
check "says the binary is already installed" said "is already $GOOD_VERSION"
check ".env, certs, dist/ unchanged" state_kept

new_case "bridge: checksum mismatch"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/badsum" run scripts/bridge-to-go.sh
expect_untouched "checksum mismatch"

new_case "bridge: bad --version"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/badver" run scripts/bridge-to-go.sh
expect_untouched "reports version 'nightly-1-gdeadbee'"

new_case "bridge: missing asset"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/missing" run scripts/bridge-to-go.sh
expect_untouched "cannot download $ASSET"

new_case "bridge: missing release (no checksums.txt)"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/nonexistent" run scripts/bridge-to-go.sh
expect_untouched "cannot download checksums.txt"

new_case "bridge: daemon-reload fails"
make_ts_box
FAKE_DAEMON_RELOAD_RC=1 ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh
check "exits non-zero" rc_nonzero
check "TS unit put back" unit_is_ts
check "no restart" not_called "systemctl restart route-agent"
check "no guard scheduled" not_called "systemd-run"

new_case "bridge guard: Go agent healthy"
cp -a "$BRIDGE_ROOT/." "$ROOT/"
run scripts/bridge-to-go.sh --guard
check "exits 0" rc_zero
check "unit stays Go" unit_is_go
check "no restart" not_called "systemctl restart"

new_case "bridge guard: Go agent crash-looping"
cp -a "$BRIDGE_ROOT/." "$ROOT/"
FAKE_GO_HEALTHY=0 run scripts/bridge-to-go.sh --guard
check "exits 0" rc_zero
check "TS unit restored" unit_is_ts
check "daemon-reload then restart route-agent" order "systemctl daemon-reload" "systemctl restart route-agent"

new_case "bridge guard: active but restarted 5 times"
cp -a "$BRIDGE_ROOT/." "$ROOT/"
FAKE_NRESTARTS=5 run scripts/bridge-to-go.sh --guard
check "TS unit restored" unit_is_ts

new_case "bridge guard: rollback returns the checkout to the pre-update commit"
make_ts_box
# The update chain ran `git reset --hard @{u}`: HEAD is the cutover commit, ORIG_HEAD the old one.
g() { git -C "$ROOT/opt/route-agent" -c user.name=t -c user.email=t@t -c commit.gpgsign=false "$@"; }
printf 'dist/\n.env\nbin/\n' > "$ROOT/opt/route-agent/.gitignore"
echo old > "$ROOT/opt/route-agent/marker"
g init -q -b main && g add -A && g commit -qm old
OLD_COMMIT="$(g rev-parse HEAD)"
echo cutover > "$ROOT/opt/route-agent/marker"
g commit -qam cutover
g update-ref ORIG_HEAD "$OLD_COMMIT"
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh
check "bridge exits 0" rc_zero
check "pre-update commit recorded" grep -qx "$OLD_COMMIT" "$ROOT/opt/route-agent/bin/ts-rollback-commit"
FAKE_GO_HEALTHY=0 run scripts/bridge-to-go.sh --guard
check "TS unit restored" unit_is_ts
check "checkout back at the pre-update commit" test "$(g rev-parse HEAD)" = "$OLD_COMMIT"
check "tracked file back to its old content" grep -qx old "$ROOT/opt/route-agent/marker"
check "still on branch main" test "$(g symbolic-ref --short HEAD)" = main
check "dist/ and .env survive" state_kept

# A box the orchestrator sent to nightly: the TS agent ran its channel chain (branch alpha) from
# main, and the bridge from alpha. The guard must bring the checkout back to main, upstream
# origin/main, or the next plain TS update would pull alpha again.
new_case "bridge guard: rollback from branch alpha returns to main"
make_ts_box
ORIGIN="$ROOT.origin"
og() { git -C "$ORIGIN" -c user.name=t -c user.email=t@t -c commit.gpgsign=false "$@"; }
mkdir -p "$ORIGIN"
og init -q -b main
printf 'dist/\n.env\nbin/\n' > "$ORIGIN/.gitignore"
echo old > "$ORIGIN/marker"
og add -A && og commit -qm old
OLD_COMMIT="$(og rev-parse HEAD)"
og checkout -q -b alpha && echo nightly > "$ORIGIN/marker" && og commit -qam nightly && og checkout -q main
g init -q -b main && g remote add origin "$ORIGIN" && g fetch -q origin && g reset -q --hard origin/main \
  && g branch -q --set-upstream-to=origin/main main
# The git part of the TS agent's SelfUpdate with x-agent-channel: nightly (src/services/system.service.ts).
g fetch -q --tags origin +refs/heads/alpha:refs/remotes/origin/alpha && g checkout -q -f -B alpha HEAD \
  && g branch -q --set-upstream-to=origin/alpha alpha && g reset -q --hard '@{u}' && g clean -qfd
check "TS chain: checkout on alpha" test "$(g symbolic-ref --short HEAD)" = alpha
check "TS chain: ORIG_HEAD is the pre-update commit" test "$(g rev-parse ORIG_HEAD)" = "$OLD_COMMIT"
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh --channel nightly
check "bridge exits 0" rc_zero
check "pre-update commit recorded" grep -qx "$OLD_COMMIT" "$ROOT/opt/route-agent/bin/ts-rollback-commit"
check "bin/channel = nightly" channel_is nightly
FAKE_GO_HEALTHY=0 run scripts/bridge-to-go.sh --guard
check "guard exits 0" rc_zero
check "TS unit restored" unit_is_ts
check "back on branch main" test "$(g symbolic-ref --short HEAD)" = main
check "main at the pre-update commit" test "$(g rev-parse HEAD)" = "$OLD_COMMIT"
check "upstream of main is origin/main" test "$(g rev-parse --abbrev-ref 'main@{u}')" = origin/main
check "tracked file back to its old content" grep -qx old "$ROOT/opt/route-agent/marker"
check "dist/ and .env survive" state_kept

new_case "bridge guard: rollback from alpha without origin/main"
make_ts_box
printf 'dist/\n.env\nbin/\n' > "$ROOT/opt/route-agent/.gitignore"
echo old > "$ROOT/opt/route-agent/marker"
g init -q -b alpha && g add -A && g commit -qm old
OLD_COMMIT="$(g rev-parse HEAD)"
echo nightly > "$ROOT/opt/route-agent/marker"
g commit -qam nightly
g update-ref ORIG_HEAD "$OLD_COMMIT"
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run scripts/bridge-to-go.sh --channel nightly
FAKE_GO_HEALTHY=0 run scripts/bridge-to-go.sh --guard
check "guard exits 0" rc_zero
check "TS unit restored" unit_is_ts
check "on branch main at the pre-update commit" test "$(g symbolic-ref --short HEAD) $(g rev-parse HEAD)" = "main $OLD_COMMIT"
check "no upstream set" test -z "$(g config --get branch.main.merge || true)"

# --- install-go.sh --convert -----------------------------------------------------------------------
section "install-go.sh --convert"

new_case "convert: success"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run install-go.sh --convert
check "exits 0" rc_zero
check "unit runs the Go binary" unit_is_go
check "TS unit kept as backup" backup_is_ts
check "binary installed, --version = $GOOD_VERSION" bin_is_good
check "stop → daemon-reload → start" order "systemctl stop route-agent" "systemctl daemon-reload"
check "daemon-reload before start" order "systemctl daemon-reload" "systemctl start route-agent"
check "checked is-active" called "systemctl is-active route-agent"
check ".env, certs, dist/ untouched" state_kept
check "insecure sudoers rule removed" sudoers_gone
check "bin/channel = nightly (default channel on branch alpha)" channel_is nightly
check "no temp files left" no_temp_left
CONVERT_ROOT="$ROOT"

new_case "convert: --channel stable"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run install-go.sh --convert --channel stable
check "exits 0" rc_zero
check "bin/channel = stable" channel_is stable

new_case "convert: default release URL per channel (no override)"
make_ts_box
PATH="$FAKECURL:$PATH" run install-go.sh --convert
check "default channel fails without the network" rc_nonzero
check "default channel (nightly) asks $NIGHTLY_URL/checksums.txt" called "$NIGHTLY_URL/checksums.txt"
PATH="$FAKECURL:$PATH" run install-go.sh --convert --channel stable
check "stable asks $STABLE_URL/checksums.txt" called "$STABLE_URL/checksums.txt"
check "box untouched" unit_is_ts

new_case "convert: Go agent does not come up → rollback to TS"
make_ts_box
FAKE_GO_HEALTHY=0 ROUTE_AGENT_RELEASE_BASE="$BASE/good" run install-go.sh --convert
check "exits non-zero" rc_nonzero
check "TS unit restored" unit_is_ts
check "TS agent started again (two starts in the log)" test "$(grep -c 'systemctl start route-agent' "$LOG")" -eq 2
check "says the TS agent is restored" said "TypeScript agent restored"
check "shows the Go agent's journal" said "(fake journal of -u route-agent"
check ".env, certs, dist/ untouched" state_kept

new_case "convert: checksum mismatch"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/badsum" run install-go.sh --convert
expect_untouched "checksum mismatch"

new_case "convert: bad --version"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/badver" run install-go.sh --convert
expect_untouched "reports version 'nightly-1-gdeadbee'"

new_case "convert: missing asset"
make_ts_box
ROUTE_AGENT_RELEASE_BASE="$BASE/missing" run install-go.sh --convert
expect_untouched "cannot download $ASSET"

new_case "rollback after a successful convert"
cp -a "$CONVERT_ROOT/." "$ROOT/"
run install-go.sh --rollback
check "exits 0" rc_zero
check "TS unit back" unit_is_ts

# --- install-go.sh fresh -----------------------------------------------------------------------------
section "install-go.sh (fresh install)"
PEM_KEY=$'-----BEGIN PRIVATE KEY-----\nMIIfake\n-----END PRIVATE KEY-----\n'
SECRET_HEX="$(printf 'b%.0s' $(seq 1 64))"
BUNDLE="$(jq -cn --arg ca $'CA-PEM\nline2\n' --arg cert $'CERT-PEM\n' --arg key "$PEM_KEY" --arg secret "$SECRET_HEX" \
  '{ca: $ca, cert: $cert, key: $key, secret: $secret}' | base64 -w0)"

new_case "fresh: secret bundle"
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run install-go.sh --secret "$BUNDLE" --port 9443 --domain front.example --decoy-port 8443
check "exits 0" rc_zero
check "agent.key written verbatim, mode 600" bash -c '[[ "$(stat -c %a "$1")" == 600 ]] && cmp -s "$1" <(printf "%s" "$2")' _ "$ROOT/etc/route-agent/certs/agent.key" "$PEM_KEY"
check "ca.crt written verbatim" cmp -s "$ROOT/etc/route-agent/certs/ca.crt" <(printf 'CA-PEM\nline2\n')
check ".env mode 600" test "$(stat -c %a "$ROOT/opt/route-agent/.env")" = 600
check ".env has PORT=9443 and the secret from the bundle" bash -c 'grep -qx "PORT=9443" "$1" && grep -qx "EGRESS_CONTROL_SECRET=$2" "$1" && grep -qx "RELOAD_COMMAND=systemctl reload sing-box" "$1" && [[ $(wc -l < "$1") -eq 8 ]]' _ "$ROOT/opt/route-agent/.env" "$SECRET_HEX"
check "unit runs the Go binary" unit_is_go
check "binary installed" bin_is_good
check "bin/channel = nightly (default channel on branch alpha)" channel_is nightly
check "enable + restart" order "systemctl enable route-agent" "systemctl restart route-agent"
check "ufw opens the agent port" called "ufw allow 9443/tcp"
check "no sudoers rule written" test ! -e "$ROOT/etc/sudoers.d/route-agent-ufw"
check "sing-box base config created" test -f "$ROOT/etc/sing-box/config.json"
check "domain echoed" said "domain: front.example, decoy port: 8443"
check "secret never on a command line of a recorded tool" not_called "$SECRET_HEX"

new_case "fresh: raw secret, existing sing-box config kept"
mkdir -p "$ROOT/etc/sing-box"
echo '{"pushed":"by orchestrator"}' > "$ROOT/etc/sing-box/config.json"
ROUTE_AGENT_RELEASE_BASE="$BASE/good" run install-go.sh --secret "$SECRET_HEX" --channel stable
check "exits 0" rc_zero
check "bin/channel = stable" channel_is stable
check ".env has the raw secret and PORT=8081" bash -c 'grep -qx "EGRESS_CONTROL_SECRET=$2" "$1" && grep -qx "PORT=8081" "$1"' _ "$ROOT/opt/route-agent/.env" "$SECRET_HEX"
check "no certificates written" test ! -e "$ROOT/etc/route-agent/certs/agent.key"
check "existing sing-box config not overwritten" grep -q 'by orchestrator' "$ROOT/etc/sing-box/config.json"

new_case "fresh: missing asset writes nothing of the agent"
ROUTE_AGENT_RELEASE_BASE="$BASE/missing" run install-go.sh --secret "$BUNDLE"
check "exits non-zero" rc_nonzero
check "no .env" test ! -e "$ROOT/opt/route-agent/.env"
check "no unit" test ! -e "$(UNIT_F)"
check "no certificates" test ! -e "$ROOT/etc/route-agent/certs/agent.key"
check "no channel file" no_channel

new_case "CLI errors"
run install-go.sh --channel preview --secret x
check "--channel preview (old name) refused" rc_nonzero
run install-go.sh --bogus
check "unknown flag → exit 1" test "$RC" -eq 1
run install-go.sh --port
check "flag without a value refused" rc_nonzero
run install-go.sh
check "missing --secret refused" said "--secret parameter is required"
run scripts/bridge-to-go.sh --bogus
check "bridge: unknown argument refused" rc_nonzero
run scripts/bridge-to-go.sh --channel preview
check "bridge: --channel preview (old name) refused" rc_nonzero
check "bridge: says which channels exist" said "--channel must be stable or nightly"
run scripts/bridge-to-go.sh --channel
check "bridge: --channel without a value refused" rc_nonzero

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
