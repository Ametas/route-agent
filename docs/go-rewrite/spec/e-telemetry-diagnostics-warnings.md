# Group E — Observability: telemetry, diagnostics, node warnings, WARP key health

Behavioral spec of the TypeScript agent (`route-agent`, branch `go-rewrite`) for the Go rewrite.
Scope: RPCs `StreamTelemetry`, `GetTelemetry`, `PullNodeWarnings`, `RunNetworkDiagnostic`,
`GetWarpKeyHealth`, their helper modules, and the background AWG health/self-heal timer.

Citations: agent paths are relative to `/home/user/route-agent`; orchestrator paths are prefixed `orch:`
(relative to `/home/user/vpn-orchestrate`). Line numbers are as of this branch.

Conventions used below:

- **shell(...)** = Node `child_process.exec` (runs through `/bin/sh -c`), promisified (`src/utils/exec.ts:4`).
  **execFile(bin, [args])** = no shell (`src/utils/exec.ts:5`). Unless stated otherwise **neither has a
  timeout** and both use Node's default `maxBuffer` of 1 MiB (stdout or stderr overflow → child killed,
  promise rejected). A non-zero exit code rejects the promise.
- "test hook" = a `NODE_ENV === 'test'` short-circuit. A Go port does not need these env vars but needs
  equivalent injection seams to reproduce the tests listed in the parity checklists.

---

## 0. Shared plumbing relevant to every RPC here

### 0.1 Auth (`src/middleware/auth.ts`)

`authenticateCall(call)` (`auth.ts:31-39`):
1. Read gRPC metadata key `x-orchestrator-secret`, first value, stringified (`auth.ts:23-26`). If it verifies → OK.
2. Else read `call.request.orchestratorSecret` or `call.request.orchestrator_secret` (payload field 1 of every
   request message in scope) and verify that.
3. `verifySecret` (`auth.ts:7-18`): reject non-string/empty token; reject if `EGRESS_CONTROL_SECRET` is empty;
   reject on byte-length mismatch; otherwise `crypto.timingSafeEqual` on UTF-8 bytes.

The orchestrator always sends BOTH metadata and payload secret (`orch:src/core/services/grpc/GrpcTelemetryStreamManager.ts:774-775,948-949`,
`orch:src/core/services/grpc/GrpcConfigPusher.ts:517,542`, `orch:src/core/services/grpc/GrpcNetworkDiagnosticClient.ts:52-57`;
metadata built in `orch:src/core/services/grpc/GrpcConnectionManager.ts:329-333`). `tests/app.test.ts:352` and
`:1670` open `StreamTelemetry` with **payload secret only, no metadata** — payload auth must work for streams too.

Auth failure behavior differs per RPC (gRPC error vs. OK response with sentinel body) — see each RPC.

### 0.2 Proto loading / field naming

Server loads proto with `keepCase:false, longs:String, enums:String, defaults:true, oneofs:true` (`src/index.ts:51-57`).
Response objects therefore must use camelCase keys; snake_case keys are **silently dropped** by proto-loader
(`src/services/nodeWarnings.service.ts:15-19`, guard test `tests/responseFieldNaming.test.ts:168-243`). For Go
(generated structs) this whole class of bug disappears; the only parity obligation is that every field below is
actually populated. The orchestrator client uses the same loader options (`orch:src/core/services/grpc/GrpcConnectionManager.ts:17-23`),
so on its side all int64/uint64 fields arrive as **decimal strings** (`timestamp`, `hostNetRxBytes`, `hostNetTxBytes`,
`measuredAtUnixMs`, `lastSeenUnixMs`) and all unset scalars as proto3 defaults.

### 0.3 Server transport settings (affect stream lifecycle)

`src/index.ts:126-145`: `grpc.keepalive_time_ms=10000`, `grpc.keepalive_timeout_ms=20000` (must mirror the
orchestrator client, `orch:src/core/services/grpc/GrpcConnectionManager.ts:294-308`; "менять его надо ОДНОВРЕМЕННО С
ОБЕИХ СТОРОН" `src/index.ts:136-139`), `grpc.keepalive_permit_without_calls=1`,
`grpc.http2.min_ping_interval_without_data_ms=5000`, `grpc.http2.max_pings_without_data=0`. mTLS is mandatory
(`src/index.ts:65-116`, out of scope here).

### 0.4 Handler registration (`src/index.ts:147-180`)

`streamTelemetry`, `getTelemetry`, `runNetworkDiagnostic`, `getWarpKeyHealth`, `pullNodeWarnings` on
`agent.EgressAgentService`. `startAwgHealthCheckTimer()` is called in `startServer()` **before** mTLS credentials
are loaded and before bind (`src/index.ts:123`).

---

## 1. Proto messages (`proto/agent.proto`)

| Message | Lines |
|---|---|
| `TelemetryRequest { string orchestrator_secret = 1; }` | 252-254 |
| `TelemetryResponse` (fields 1-20) | 256-285 |
| `NetworkDiagnosticRequest/Probe/Hop/TargetResult/Response` | 576-620 |
| `WarpKeyHealthRequest/Entry/Response` | 743-769 |
| `NodeWarningsRequest/NodeWarning/NodeWarningsResponse` | 771-797 |

`TelemetryResponse`:

| # | field | type | source (see §2.4) |
|---|---|---|---|
| 1 | cpu_usage | double | `/proc/stat` delta, percent 0-100 |
| 2 | mem_usage | double | `/proc/meminfo`, percent |
| 3 | active_connections | uint32 | line count `/proc/net/tcp`+`/proc/net/udp` |
| 4 | system_logs | string | stream: `journalctl -u sing-box -f` tail buffer; unary: `"OK"` |
| 5 | timestamp | int64 | agent `Date.now()` (ms) at send time |
| 6 | webrtc_status | string | olcrtc-agent-srv presence + running units |
| 7 | singbox_version | string | `sing-box version` (cached 60 s) |
| 8 | awg_active_peers | uint32 | `awg show awg0 dump`, handshake ≤180 s |
| 9 | awg_version | string | **same value as field 11** (alias) |
| 10 | awg_status | string | `checkAndHealAwgInterface()` status (may restart AWG!) |
| 11 | awg_tools_version | string | `awg --version` (cached 60 s) |
| 12 | awg_go_version | string | `amneziawg-go --version` (cached 60 s) |
| 13 | mesh_awg_kernel_status | string | `modinfo amneziawg` + `lsmod` (cached 60 s) |
| 14 | mesh_awg_kernel_version | string | `modinfo` `version:` line |
| 15 | mesh_tunnel_interface_status | string | `ip link show awgmesh0` |
| 16 | mesh_tunnel_active_peers | uint32 | `awg show awgmesh0 dump` |
| 17 | mesh_tunnel_live_peer_keys | repeated string | same dump, keys of live peers |
| 18 | host_net_iface | string | `/proc/net/route` default route iface |
| 19 | host_net_rx_bytes | uint64 | `/proc/net/dev` rx bytes of that iface |
| 20 | host_net_tx_bytes | uint64 | `/proc/net/dev` tx bytes of that iface |

Note the proto comment on field 13 lists `"not_installed" | "loaded" | "error"` and on field 15 `"up" | "down" | "absent"`
(`proto/agent.proto:271,273`) — **both are inaccurate**; the real value sets are in §2.4.

---

## 2. `StreamTelemetry` (server-streaming) and `GetTelemetry` (unary)

Handler file: `src/services/telemetry.service.ts`. Collectors: `src/utils/telemetry.ts`, `src/utils/hostNetwork.ts`.

### 2.1 StreamTelemetry — lifecycle (`telemetry.service.ts:26-142`)

1. **Auth** (`:29-35`). On failure: log warn `'Unauthorized gRPC telemetry stream requested'`, then
   `call.emit('error', err)` where `err.message = 'UNAUTHENTICATED: Invalid orchestrator secret token.'`,
   `err.code = 16`. grpc-js (`@grpc/grpc-js` 1.14.4, `server-call.js:121-124` + `serverErrorToStatus`
   `:25-41`) turns this into trailers **status 16 UNAUTHENTICATED, details `"UNAUTHENTICATED: Invalid orchestrator secret token."`**
   and ends the call. No data frame is sent; no journalctl is spawned; no timers are created.
2. **Spawn log tail** (`:40`): `spawn('journalctl', ['-u', 'sing-box', '-n', '10', '-f', '--output', 'cat'])`.
   - Unit name `sing-box` is **hard-coded** (not derived from `SINGBOX_UNIT_FILE_PATH`).
   - `'error'` on the child (e.g. ENOENT) → warn log `'Failed to spawn journalctl process'`, stream continues (logs stay empty).
   - stdout `'error'` ignored (debug log). stdout `'data'` → `logBuffer += chunk.toString()`; if
     `logBuffer.length > 8192` keep the **last 8192 JS chars** (UTF-16 code units, not bytes) (`:49-54`).
     `chunk.toString()` decodes each chunk independently as UTF-8, so a multi-byte char split across chunks
     becomes U+FFFD (parity-irrelevant but observable).
   - stderr is not read (pipe left unconsumed).
3. **Per-stream state**: `logBuffer=''`, `telemetryInterval=null`, `isCleanedUp=false`,
   `streamCpuStats={idle:0,total:0}` (`:38,57-59`).
4. **cleanup()** (`:62-76`), idempotent via `isCleanedUp`: `clearInterval`; if child alive
   (`!killed && pid`) → `kill('SIGTERM')` (errors swallowed); info log
   `'Telemetry binary stream closed and resources safely released'`. Does **not** call `call.end()`.
5. Cleanup is attached to call events `cancelled`, `close`, `finish`, `error` (`:79-82`).
6. **Tick** (`sendTelemetryTick`, `:84-137`):
   - Return immediately if `isCleanedUp || call.destroyed`.
   - Run **12 collectors concurrently** (`Promise.all`, `:88-101`): `getCpuUsage(streamCpuStats)`, `getMemoryUsage`,
     `getConnectionCount`, `getWebRtcStatus`, `getSingBoxVersionCached`, `getAwgActivePeersCount`,
     `getAwgToolsVersionCached`, `getAmneziaWgGoVersionCached`, `getAwgStatus`, `getMeshAwgKernelStatusCached`,
     `getMeshTunnelInterfaceStatus`, `getHostNetwork`.
   - `streamCpuStats = newStats` (`:102`).
   - Re-check `isCleanedUp || call.destroyed` (`:104`).
   - `call.write({...})` (`:107-126`) with: `cpuUsage, memUsage, activeConnections, systemLogs: logBuffer,
     timestamp: Date.now(), webrtcStatus, singboxVersion, awgActivePeers, awgVersion: awgToolsVer, awgStatus,
     awgToolsVersion: awgToolsVer, awgGoVersion, meshAwgKernelStatus, meshAwgKernelVersion,
     meshTunnelInterfaceStatus, meshTunnelActivePeers, meshTunnelLivePeerKeys, hostNetIface, hostNetRxBytes, hostNetTxBytes`.
   - If `call.write` **throws synchronously** → `cleanup()` and return (`:127-130`). The boolean backpressure
     return value of `write` is ignored.
   - After a successful write: `logBuffer = ''` (`:132`). So `system_logs` carries exactly the journal output
     received since the previous successful frame (capped to last 8192 chars).
   - Any rejection from the `Promise.all` → debug log `'Error inside telemetry streaming interval tick'`, no frame
     this tick, buffer kept (`:133-136`). In practice every collector swallows its own errors (see §2.4), so
     frames are essentially always sent.
7. **Cadence** (`:139-141`): `await sendTelemetryTick()` (immediate first frame) **then**
   `setInterval(sendTelemetryTick, 2000)`. Consequences:
   - The interval clock starts only after the first tick completes (first-to-second frame gap = 2 s after the
     first tick finished).
   - `setInterval` does **not** wait for a previous tick: if a tick takes >2 s (no exec has a timeout), ticks
     overlap and run concurrently (shared `streamCpuStats` race — the last finisher wins).
   - The handler never ends the stream itself; it lives until the client cancels or the transport dies.
   - If the client cancels *during* the first awaited tick, cleanup runs, then `setInterval` is still created
     afterwards (`:141`) — but each later tick returns immediately on `isCleanedUp` and the interval is never cleared
     (leaked timer; see Risks).
8. **Multiple concurrent streams**: fully independent per call — each spawns its own `journalctl -f`, keeps its
   own `logBuffer` and CPU baseline. Module-level caches (versions, mesh kernel, AWG health state) are shared and
   **not** in-flight-deduplicated: N streams at a cold cache run N concurrent probes. Every stream's tick also runs
   `checkAndHealAwgInterface()` (which can `systemctl restart` AWG) — see §2.5.

### 2.2 GetTelemetry (unary) (`telemetry.service.ts:147-234`)

1. **Auth failure → OK response, not a gRPC error** (`:151-172`), warn log
   `'Unauthorized GetTelemetry Unary request blocked'`, body:
   `cpuUsage 0, memUsage 0, activeConnections 0, systemLogs 'Error: Unauthorized secret token mismatch',
   timestamp Date.now(), webrtcStatus 'unauthorized', singboxVersion 'unknown', awgActivePeers 0,
   awgVersion 'unknown', awgStatus 'unauthorized', awgToolsVersion 'unknown', awgGoVersion 'unknown',
   meshAwgKernelStatus 'unknown', meshAwgKernelVersion '', meshTunnelInterfaceStatus 'unknown',
   meshTunnelActivePeers 0, meshTunnelLivePeerKeys []` — host-net fields absent (→ `''`/`0`).
2. Success (`:174-210`): same 12 collectors in parallel, but CPU baseline is a **fresh `{idle:0,total:0}`** every
   call (`:175`), so `cpu_usage` = **average utilization since boot**, not instantaneous. `systemLogs: 'OK'`.
   Other fields identical to a stream frame. `newStats` is discarded.
3. Exception path (`:211-233`, effectively unreachable since collectors swallow errors): error log
   `'Failed to process GetTelemetry Unary request'`; OK response with `systemLogs 'Internal Error: <msg>'`,
   `webrtcStatus/singboxVersion/awgVersion/awgStatus/awgToolsVersion/awgGoVersion/meshAwgKernelStatus/meshTunnelInterfaceStatus = 'error'`,
   numerics 0, `meshAwgKernelVersion ''`, keys `[]`.
4. GetTelemetry **does** run `getAwgStatus()` → `checkAndHealAwgInterface()` → may restart AWG (§2.5).

### 2.3 What the orchestrator does with the stream (timing contracts the Go port must meet)

- Opens one stream per node with both metadata and payload secret (`orch:src/core/services/grpc/GrpcTelemetryStreamManager.ts:774-775`).
- **First-frame watchdog: 5000 ms** (`:782-795`). If no frame within 5 s after the stream is created → recorded
  error `'Превышено время ожидания gRPC отклика (TRANSIENT_FAILURE / TLS Mismatch)'`, stream torn down, reconnect
  scheduled. ⇒ **The first frame must be emitted well under 5 s**, even with cold caches (versions, modinfo) and
  even if the AWG self-heal path runs `systemctl restart` inside that first tick.
- On first frame: downtime state reset, `handleAgentTransition(..., 'up')`, contract verification, pending
  self-update check (`:812-844`). Every frame: `telemetryCache.set`, `lastUpdateTime.set(Date.now())` (orchestrator
  clock), `lastErrors.delete` (`:845-847`).
- `error`/`end` → cleanup + reconnect ladder **10 s, 30 s, 300 s** (`:65`), node declared **down on the 2nd
  consecutive drop** (`DROPS_BEFORE_DOWN = 2`, `:137`, `:447-457`), then abandoned (Xeon fronts swept every 5 min
  via unary `GetTelemetry`; egress revived by Remnawave webhook) (`:457-468`, `:533-575`).
- **Freshness**: online iff last frame received < **20 000 ms** ago by the orchestrator's clock (`:1023-1038`)
  — "Стрим шлёт кадр каждые 2 секунды, так что двадцать — это десять пропущенных подряд." (`:1037`). Agent
  `timestamp` is explicitly NOT used for freshness (clock skew incident, `:1023-1032`).
- `getTelemetry()` returns cache only if age ≤ `maxAgeMs` default **60 000** (`:636`).
- **Unary fallback** `fetchTelemetryUnary` with deadline `PROBE_DEADLINE_MS = 3000` or display `DISPLAY_PROBE_DEADLINE_MS = 1000`
  (`orch:src/shared/constants/telemetryProbe.ts`; call at `GrpcTelemetryStreamManager.ts:937-982`). A successful unary
  response (including the auth-failure sentinel body!) is cached as telemetry and marks the node online
  (`:954-959`, `:1000-1013`). ⇒ **GetTelemetry must reliably answer within 1 s.**
- `systemLogs` and `awgStatus` are **not consumed anywhere** in the orchestrator (grep of `orch:src` — zero hits outside
  `types/grpc.ts`). They must still be populated for wire parity.

### 2.4 Collectors — exact behavior

#### 2.4.1 `cpu_usage` — `getCpuUsage(prev)` (`src/utils/telemetry.ts:16-33`)
- Read `/proc/stat`, take line 0 (`cpu  user nice system idle iowait irq softirq steal guest guest_nice`).
- `times = line.split(/\s+/).slice(1).map(Number)`; `total = Σ times` (all columns, **including guest and
  guest_nice**, which the kernel already counts inside user/nice — double count); `idle = times[3]` (iowait is
  counted as busy).
- `cpuUsage = diffTotal === 0 ? 0 : ((diffTotal − diffIdle) / diffTotal) × 100`, `diffX = X − prev.X`.
- Returns `newStats = {idle,total}`. On read error → `{cpuUsage:0, newStats: prev}`.
- First frame of every stream and every GetTelemetry use prev = `{0,0}` → since-boot average.
- Units: percent, unrounded double. Orchestrator rounds/`toFixed(1)` (`orch:src/core/services/adminInfra.ts:91,136`,
  `orch:src/zones/bot/commands/admin/screens/infra.ts:122`).

#### 2.4.2 `mem_usage` — `getMemoryUsage()` (`telemetry.ts:38-51`)
- Read `/proc/meminfo`; regex `MemTotal:\s+(\d+)` and `MemAvailable:\s+(\d+)` (kB). Either missing → 0.
- `((total − avail) / total) × 100`. Read error → 0. (total=0 would give NaN; not guarded.)

#### 2.4.3 `active_connections` — `getConnectionCount()` (`telemetry.ts:56-66`)
- Read `/proc/net/tcp` then `/proc/net/udp` (sequentially). Each: `trim().split('\n').length − 1` (minus header).
- Sum, `Math.max(0, …)`. Any read error → 0.
- **IPv4 only** (no `tcp6`/`udp6`), all TCP states (LISTEN, TIME_WAIT, …) and all UDP sockets, in the agent's
  network namespace. Consumed only by `orch:src/core/services/adminFronts.ts:68`.

#### 2.4.4 `webrtc_status` — `getWebRtcStatus()` (`telemetry.ts:76-96`)
- Test hook: `NODE_ENV=test` and `TEST_WEBRTC_CHECK !== 'true'` → `'nominal'`.
- `fs.stat(OLCRTC_AGENT_SRV_BINARY_PATH)` (default `/usr/local/bin/olcrtc-agent-srv`, `src/config.ts:78`); missing → `'not_installed'`.
- shell: `systemctl list-units --type=service --state=running --no-legend 'olcrtc-agent-srv@*' | awk '{print $1}'`;
  count non-empty trimmed lines; `0 → 'no_active_tunnels'`, else `'nominal'`.
- Exec rejection → `'unavailable'` (practically only if `sh`/`awk` fails, since a pipeline's exit status is awk's).
- Not cached; runs every tick when the binary exists.
- Value set: `nominal | no_active_tunnels | not_installed | unavailable` (+ `unauthorized`/`error` from GetTelemetry
  special bodies). Orchestrator compares: `'nominal'`, `'not_installed'`, `'no_active_tunnels'`
  (`orch:src/zones/bot/commands/admin/utils/nodeHealthDisplay.ts:70-73`, `orch:src/core/services/adminInfra.ts:217-230`
  — anything else → `'error'`), and gates olcrtc provisioning on `nominal|no_active_tunnels`
  (`orch:src/zones/bot/commands/client/handlers.ts:826-836`).

#### 2.4.5 `singbox_version` — `getSingBoxVersionCached()` (`telemetry.ts:98-162`)
- **Unit gate first**: `fs.stat(SINGBOX_UNIT_FILE_PATH)` (default `/etc/systemd/system/sing-box.service`); missing →
  `'not_installed'` even if the binary works ("отсутствие ЮНИТА равносильно отсутствию ядра", `:101-110`; test
  `tests/singboxInstalledSignal.test.ts:53-65`).
- execFile(`SINGBOX_BINARY_PATH` default `/usr/local/bin/sing-box`, `['version']`); rejection → `'not_installed'`.
- `parseSingBoxVersionOutput` (`:124-127`): first of `/sing-box version (\S+)/`, `/version\s+(\S+)/i`; else first line
  trimmed; else `'not_installed'`. **The whole token up to whitespace is kept** — `1.14.0-hotusers` must stay
  intact (`:118-123`; `tests/singboxInstalledSignal.test.ts:67-80`).
- Cache: module-level, TTL **60 000 ms**, refreshed when cache empty or stale (`:146-157`).
  `invalidateSingboxVersionCache()` (`:159-162`) is called after binary replace/upgrade
  (`src/services/binary.service.ts:199,361`). Not invalidated when the unit is removed (staleness ≤60 s).
- Orchestrator: `!== 'not_installed'` ⇒ installed (`orch:src/core/services/orchestrator/nodeOrchestrator.ts:49-51`,
  `orch:src/core/SingBoxService.ts:155`); `classifyVersionStatus` treats `''|'not_installed'|'unknown'` as not installed,
  `'present'|'installed'` as unversioned (`orch:src/shared/utils/versionStatus.ts:30-38`); fork detection by suffix
  `-hotusers` and `<base>-<rev>-hotusers` (`orch:src/shared/constants/singboxFork.ts:20,50-66`), feature gates
  `supportsProxyProtocol`/`supportsTransparentSource` parse the base version (`:89-125`).

#### 2.4.6 `awg_active_peers` — `getAwgActivePeersCount()` (`telemetry.ts:167-200`)
- Test hooks: `TEST_AWG_PEERS` unset → 0; set → parseInt.
- shell: `awg show awg0 dump` — interface name **hard-coded `awg0`** (not `getAwgInterfaceName()`), `awg` from `$PATH`.
- Skip line 0 (interface line). For each peer line split on whitespace, if ≥5 columns: `latestHandshake =
  parseInt(col[4])`; count if `!NaN && >0 && (nowSec − hs) ≤ 180`.
- ≤1 line → 0. Any error → 0 (debug log).

#### 2.4.7 `awg_tools_version` (and `awg_version`) — `getAwgToolsVersionCached()` (`telemetry.ts:225-266,307-319`)
- Test hooks: `TEST_AWG_TOOLS_VERSION`, then `TEST_AWG_VERSION`.
- Candidates, de-duplicated in order: `AWG_TOOLS_BINARY_PATH` (default `/usr/local/bin/awg`), `/usr/local/bin/awg`, `/usr/bin/awg`.
  For each: execFile(bin, `['--version']`); `out = (stdout || stderr).trim()`; if `out.toLowerCase()` contains
  `amneziawg-go` → return `'not_installed'` immediately (a legacy symlink of `awg` → `amneziawg-go`; cf.
  `sanitizeAwgToolsSymlink`, `src/services/binary.service.ts:278-282`); else `parseAwgVersionString(out)`; non-null → return.
  Errors swallowed, next candidate.
- Fallback: shell `awg --version` with the same two checks.
- Else `'not_installed'`.
- `parseAwgVersionString` (`:206-220`), first match wins:
  1. `/amneziawg-go\s+v?([0-9\.]+[\w\-]*)/i`
  2. `/amneziawg-go\s+v?([0-9\.]+)/i`
  3. `/v?([0-9\.]+\-[0-9a-zA-Z]+)/i`
  4. `/v?([0-9]+\.[0-9]+[0-9a-zA-Z.-]*)/i`
  5. `/([0-9\.]+)/`
  then strip leading `v`, strip trailing `[\s.-]+`, trim; reject empty or `'.'` → `null`. Expected values in
  `tests/app.test.ts:1992-2011` (e.g. `'awg v1.0.20260618-2\n' → '1.0.20260618-2'`, `'no_version_here' → null`).
- `awg_version` (field 9) is always the same string as `awg_tools_version` (`telemetry.service.ts:116,118`).
- Orchestrator: tools installed iff trimmed value not in `{'', 'not_installed', 'unknown'}`
  (`orch:src/core/services/orchestrator/meshTunnelOrchestrator.ts:199-200`); display falls back
  `awgToolsVersion || awgVersion` (`orch:src/core/services/adminInfra.ts:338`).

#### 2.4.8 `awg_go_version` — `getAmneziaWgGoVersionCached()` (`telemetry.ts:271-298,321-329`)
- Test hook `TEST_AMNEZIAWG_GO_VERSION`.
- Candidates: `AWG_GO_BINARY_PATH` (default `/usr/local/bin/amneziawg-go`), `/usr/local/bin/amneziawg-go`. execFile `--version`;
  `out = (stdout||stderr).trim()`; regex 1 or 2 above → return `match[1]` without leading `v`, trimmed (note: no
  trailing-punctuation strip on this branch); else `parseAwgVersionString(out)`. All fail → `'not_installed'`.
- Orchestrator: `awgGoInstalled = value && value !== 'not_installed'` (`orch:src/core/services/orchestrator/nodeOrchestrator.ts:1370`);
  `resolveAwgClientCapability`: empty/`'unknown'` → `'unseen'`, `'not_installed'` → `'no_go_core'`, else `'capable'`
  (`orch:src/core/services/orchestrator/awgOrchestrator.ts:684-691`).
- **Cache quirk**: tools and go caches share ONE timestamp `lastAwgCheckTime` (`telemetry.ts:307-329`). Because both
  are called in the same `Promise.all`, both read the timestamp before either refresh writes it, so in practice they
  refresh together every 60 s. `invalidateAwgVersionCache()` clears both (`:335-339`), called after awg-tools/awg-quick/
  amneziawg-go uploads (`src/services/binary.service.ts:497,604`).

#### 2.4.9 `awg_status` — `getAwgStatus()` (`telemetry.ts:623-629`) → see §2.5.

#### 2.4.10 `mesh_awg_kernel_status` / `mesh_awg_kernel_version` — `getMeshAwgKernelStatusCached()` (`telemetry.ts:356-394`)
- Test hooks `TEST_MESH_AWG_KERNEL_STATUS`, `TEST_MESH_AWG_KERNEL_VERSION`.
- shell `modinfo amneziawg`; rejection → `{status:'not_installed', version:''}`. Version: `/^version:\s*(\S+)/m` or `''`.
- shell `lsmod | grep -w amneziawg`; success → `'loaded'`, rejection → `'built_not_loaded'` (both with the modinfo version).
- Rationale: "modinfo reads the on-disk/depmod module regardless of load state … 'built_not_loaded' … distinguishable
  from 'not_installed'" (`:349-355`). Mesh is **never auto-healed** by the agent (`:343-347`).
- Cache TTL 60 000 ms; `invalidateMeshAwgKernelStatusCache()` after DKMS install (`src/services/meshTunnel.service.ts:262`).
- Real value set: `loaded | built_not_loaded | not_installed` (+ `unknown`/`error` from GetTelemetry special bodies).
- Orchestrator: `=== 'loaded'` ⇒ installed (`orch:src/core/services/orchestrator/meshTunnelOrchestrator.ts:202-204`);
  `{'not_installed','built_not_loaded'}` ⇒ broken → alert to system channel + egress self-repair op
  `mesh_provision` at most once per 24 h; `'loaded'` → recovery message; anything else (incl. empty) → no signal
  (`orch:src/core/NodeWarningWorker.ts:104-126,293-335`). Display mapping `loaded/built_not_loaded/not_installed`
  (`orch:src/zones/bot/utils/formatters.ts:152-164`).

#### 2.4.11 `mesh_tunnel_interface_status` / `_active_peers` / `_live_peer_keys` — `getMeshTunnelInterfaceStatus()` (`telemetry.ts:438-461`)
- Test hooks `TEST_MESH_TUNNEL_STATUS`, `TEST_MESH_TUNNEL_ACTIVE_PEERS`, `TEST_MESH_TUNNEL_LIVE_PEER_KEYS` (comma list).
- iface = basename of `MESH_AWG_CONFIG_PATH` without `.conf` (default `awgmesh0`; `src/utils/awg.ts:22-26`).
- shell `ip link show <iface>`; rejection → `{'absent', 0, []}`.
- shell `awg show <iface> dump`; rejection → `{'up', 0, []}`; else `{'up', ...parseMeshPeerDump(stdout, nowSec)}`.
- `parseMeshPeerDump` (`:418-430`): skip line 0; split each line on whitespace; skip if <5 columns; live if
  `hs = parseInt(col[4])`, `!NaN && hs > 0 && now − hs ≤ 180` (`MESH_HANDSHAKE_FRESH_SEC`, `:397`) → push `col[0]`
  (public key, base64 incl. `=`). `activePeers = livePeerKeys.length`. Order = dump order. Tests
  `tests/meshPeerDump.test.ts:22-35` (exactly 180 s old is live, 181 s is not).
- Not cached; runs every tick. Value set: `up | absent` (never `down` despite proto comment).
- Orchestrator: `liveMeshPeersOf` — `'absent'|'down'` → empty set; non-`'up'` → unknown (null); `'up'` with
  `keys.length === 0 && activePeers > 0` → treated as an old agent (null) (`orch:src/shared/utils/meshRouting.ts:29-40`).
  ⇒ **activePeers must equal livePeerKeys.length** or rings get mis-gated. Ring pair offered only if the node's
  tunnel public key is in the front's live set (`:52-60`).

#### 2.4.12 `host_net_*` — `getHostNetwork()` / `hostNetworkFields()` (`src/utils/hostNetwork.ts`)
- Read `/proc/net/route` and `/proc/net/dev` concurrently (`:48-56`).
- `defaultRouteInterface` (`:21-32`): skip header; split on whitespace; need ≥8 columns; `Destination == '00000000'`;
  `flags = parseInt(col[3], 16)`, require `flags & 0x1` (RTF_UP); `metric = parseInt(col[6]) || 0`; pick the
  **lowest metric**, ties keep the first seen. IPv4 only.
- `interfaceCounters` (`:35-45`): find a line whose text before the first `:` trims to the iface name; split the rest
  on whitespace; `rx = cols[0]`, `tx = cols[8]`; both must be finite numbers.
- Any failure / no default route / iface not in dev → `null` → fields `{hostNetIface:'', hostNetRxBytes:0, hostNetTxBytes:0}` (`:60-62`).
- Tests `tests/hostNetwork.test.ts:10-41` (eth0 metric 100 beats eth1 metric 200; non-default `awg-mesh` and `docker0` ignored).
- Orchestrator `hostTrafficOf` (`orch:src/core/services/hostTraffic.ts:24-45`): empty iface → no data; keeps the last
  two samples **keyed by agent `timestamp`** (same timestamp = same sample, not a new one); rate = Δbytes/Δt where
  Δt uses the agent timestamp (normalized: >1e11 treated as ms, `orch:src/shared/utils/grpcTimestamp.ts:29-34`);
  no rate if Δt ≤0, Δt >10 min, or a counter decreased (reboot). ⇒ `timestamp` must be epoch **milliseconds** and
  strictly increase between frames.

#### 2.4.13 `timestamp`
`Date.now()` (ms since epoch) evaluated when building the frame (`telemetry.service.ts:112,158,196`).

### 2.5 AWG health check / self-heal (`telemetry.ts:463-629`)

State: module-level map `awgHealthStates[iface] = {lastCheckTime, lastAutoRestartTime, autoRestartCount, status}`
(`:463-470`). Default iface = `getAwgInterfaceName()` = basename of `AWG_CONFIG_PATH` (default `awg0`, `src/utils/awg.ts:12-16`).

`checkAndHealAwgInterface(iface)` (`:476-590`):
1. Start from existing state or `{0,0,0,'unknown'}`.
2. Test hook `TEST_AWG_STATUS` → set status, return.
3. **Configured?** `fs.stat(config.AWG_CONFIG_PATH || /etc/amnezia/amneziawg/${iface}.conf)` exists **OR**
   shell `systemctl is-enabled route-awg@${iface}` stdout trimmed `=== 'enabled'` (non-zero exit → false)
   (`:492-511`). Note the path uses `AWG_CONFIG_PATH`, not the `iface` argument. Not configured → status
   `'inactive'`, return.
4. **Alive?** shell `ip link show ${iface}` succeeds (`:520-531`).
5. If not alive (`:535-558`): warn log; if `lastAutoRestartTime === 0 || now − lastAutoRestartTime ≥ 300 000`:
   shell `systemctl restart route-awg@${iface}` (awaited, no timeout). On success: `lastAutoRestartTime = now`,
   `autoRestartCount++`, info log. On failure: error log, **nothing recorded → no cooldown** (retried on the very next
   call). Within cooldown: warn log with `waitSec = ceil((300000 − elapsed)/1000)`.
   Note: `isAlive` is not re-evaluated after the restart.
6. Status string (`:560-585`): if `lastAutoRestartTime > 0`: `elapsedMins = floor(elapsedMs/60000)`;
   `<60 → "restarted ${max(0,elapsedMins)}m ago"`, else `"restarted ${floor(mins/60)}h ago"`.
   - alive: `"nominal (restarted Xm ago)"` if restart info exists and restart < 24 h ago, else `"nominal"`.
   - dead: `"crashed (restarted …)"` if any restart ever recorded (no 24 h limit), else `"crashed"`.
7. `lastCheckTime = now`; store.

Value set of `awg_status`: `inactive | nominal | nominal (restarted Nm ago) | nominal (restarted Nh ago) | crashed |
crashed (restarted Nm ago) | crashed (restarted Nh ago)` (+ `unauthorized`/`error` from GetTelemetry special bodies).

Callers / triggers:
- **Background timer** `startAwgHealthCheckTimer(intervalMs = 30000)` (`:603-614`): no-op if already running or
  `NODE_ENV=test`; plain `setInterval` (not unref'd, ticks may overlap), errors → debug log. Started once in
  `startServer()` (`src/index.ts:123`). `stopAwgHealthCheckTimer()` exists (`:616-621`) but is not called anywhere in `src/`.
- **Every telemetry frame** (each stream, every 2 s) and **every GetTelemetry** via `getAwgStatus()` (`:623-629`).
  So the effective check rate is 1/30 s + 1/2 s per open stream; the restart rate is bounded only by the 5-min
  cooldown in shared state.
- `getAwgHealthState(iface)` (`:592-599`) returns stored state or `{0,0,0,'inactive'}`; no callers in `src/` besides tests.

Test: `tests/app.test.ts:764-793` (alive → `nominal`; down → restart count 1 and `crashed (restarted…`; immediate
re-check → count stays 1; recovered → starts with `nominal (restarted`).

### 2.6 Per-tick external-process inventory (stream, warm cache)

| Command | Mode | When |
|---|---|---|
| `journalctl -u sing-box -n 10 -f --output cat` | spawn, long-lived | once per stream |
| `systemctl list-units --type=service --state=running --no-legend 'olcrtc-agent-srv@*' \| awk '{print $1}'` | shell | every tick if olcrtc binary exists |
| `awg show awg0 dump` | shell | every tick |
| `systemctl is-enabled route-awg@awg0` | shell | every tick if `AWG_CONFIG_PATH` file absent |
| `ip link show awg0` | shell | every tick if AWG configured |
| `systemctl restart route-awg@awg0` | shell | when AWG configured, iface missing, cooldown elapsed |
| `ip link show awgmesh0` | shell | every tick |
| `awg show awgmesh0 dump` | shell | every tick if iface exists |
| `sing-box version` | execFile | ≤ once/60 s, only if unit file exists |
| `<awg> --version` ×≤3, `awg --version` (shell) | execFile/shell | ≤ once/60 s |
| `<amneziawg-go> --version` ×≤2 | execFile | ≤ once/60 s |
| `modinfo amneziawg`, `lsmod \| grep -w amneziawg` | shell | ≤ once/60 s |

Files read every tick: `/proc/stat`, `/proc/meminfo`, `/proc/net/tcp`, `/proc/net/udp`, `/proc/net/route`,
`/proc/net/dev`; `stat()` of `OLCRTC_AGENT_SRV_BINARY_PATH`, `AWG_CONFIG_PATH`; `SINGBOX_UNIT_FILE_PATH` (when cache stale).

### 2.7 Parity checklist — telemetry

1. Stream with bad secret → gRPC status `UNAUTHENTICATED (16)`, details `"UNAUTHENTICATED: Invalid orchestrator secret token."`, zero data frames, no child spawned.
2. Stream accepts the secret from metadata `x-orchestrator-secret` OR from payload `orchestrator_secret` (payload-only works).
3. First frame is sent immediately (before any interval), and in < 5 s under cold caches; subsequent frames every 2 s (measured from end of the first tick).
4. `system_logs` in frame N = journal output received since frame N−1 (cleared only after a successful send), truncated to the last 8192 characters; first frame may contain the `-n 10` backlog.
5. `journalctl` child is started per stream and SIGTERM'ed on cancel/close/finish/error; failure to spawn does not break the stream.
6. N concurrent streams → N independent children, buffers and CPU baselines.
7. `cpu_usage` per stream is the delta since that stream's previous tick using the exact formula of §2.4.1 (including guest double counting and iowait-as-busy); first frame and every GetTelemetry use a zero baseline (since-boot average).
8. `mem_usage` = `(MemTotal−MemAvailable)/MemTotal×100`; 0 if either line missing.
9. `active_connections` = (lines of `/proc/net/tcp` − 1) + (lines of `/proc/net/udp` − 1), IPv4 only, ≥ 0.
10. `webrtc_status` ∈ {`not_installed` (no binary), `no_active_tunnels` (0 running `olcrtc-agent-srv@*`), `nominal`, `unavailable`}.
11. `singbox_version` = `not_installed` if the unit file is missing even with a working binary; full version token preserved (`1.14.0-hotusers`, `1.15.0-beta.1`); 60 s cache; invalidated on binary replace.
12. `awg_version == awg_tools_version` always.
13. Tools version returns `not_installed` if any `--version` output contains `amneziawg-go` (case-insensitive); candidate order and parsing per §2.4.7 (test vectors in `tests/app.test.ts:1995-2002`).
14. `awg_active_peers` counts peers of `awg0` with `0 < now−hs ≤ 180`.
15. Mesh kernel: `not_installed` iff `modinfo amneziawg` fails; else `loaded`/`built_not_loaded` by `lsmod`; version from `version:` line; 60 s cache; invalidated after DKMS install.
16. Mesh iface: `absent` iff `ip link show` fails; else `up`; live keys = peers with `0 < now−hs ≤ 180` in dump order; `active_peers == len(live_peer_keys)` (dump failure → `up`, 0, []).
17. Host net: lowest-metric UP default route, IPv4; rx/tx bytes columns 0 and 8 of `/proc/net/dev`; no data → `''`,0,0.
18. `timestamp` epoch ms, strictly increasing per stream.
19. `awg_status` strings exactly as §2.5; AWG restart via `systemctl restart route-awg@<iface>` at most once per 5 min per iface (successful restarts only), triggered by the 30 s timer AND by every telemetry tick / GetTelemetry.
20. GetTelemetry bad secret → **OK** response with the exact sentinel body of §2.2 (`webrtcStatus 'unauthorized'`, `awgStatus 'unauthorized'`, versions `'unknown'`, mesh status `'unknown'`, `systemLogs 'Error: Unauthorized secret token mismatch'`).
21. GetTelemetry success → `systemLogs == "OK"`; answers within 1 s (orchestrator display deadline).

---

## 3. `PullNodeWarnings` (unary)

Handler: `src/services/nodeWarnings.service.ts:22-84`. Sources: `src/utils/journalWarnings.ts`,
`src/utils/listenerWarning.ts`, `src/utils/rearRuleProviderWarning.ts`.

### 3.1 Inputs / auth
`NodeWarningsRequest { orchestrator_secret }`. Auth failure → **OK** response
`{success:false, message:'Invalid orchestrator secret token.', warnings:[], lookbackHours:0}`, warn log (`:26-33`).

### 3.2 Flow
`Promise.all([collectJournalWarnings(), detectCoreNotListening(), detectEmptyRearRuleProviders()])` (`:44-50`);
`warnings = [notListening, emptyProviders, ...journal].filter(nonNull)` (`:51-53`) — **order: listener warning,
rear-provider warning, then journal warnings sorted by lastSeen desc**. If non-empty: info log with `kind@source`
list. Response (`:62-73`):
`success:true, message: n===0 ? 'No degradation warnings in the journal.' : '${n} warning class(es) found.',
warnings:[{kind,source,sample,occurrences,lastSeenUnixMs}], lookbackHours: NODE_WARNING_LOOKBACK_HOURS`.
Catch-all (`:74-83`, effectively unreachable): `success:false, message:'Internal Agent Error: <msg>'`, empty list,
`lookbackHours` = config value.

### 3.3 Journal scan — `collectJournalWarnings(runExecFile, lookbackHours=NODE_WARNING_LOOKBACK_HOURS (24), now)` (`journalWarnings.ts:233-299`)

Pattern registry (`:39-62`), **first match wins, order matters**:

| kind | regex (used verbatim for `--grep` AND classification) |
|---|---|
| `quic_recv_buffer` | `failed to sufficiently increase receive buffer size` |
| `quic_send_buffer` | `failed to sufficiently increase send buffer size` |
| `quic_buffer_not_increased` | `failed to increase (?:receive\|send) buffer size` |
| `conntrack_table_full` | `nf_conntrack: table full` |
| `too_many_open_files` | `too many open files` |
| `out_of_socket_memory` | `Out of socket memory` |
| `neighbour_table_overflow` | `neighbour table overflow` |

- `buildGrepPattern` = `patterns.map(p => '(?:' + p.regex + ')').join('|')` (`:101-103`).
- `sinceArgument(h, now)` = `now − h·3600 s` formatted **in local time** as `YYYY-MM-DD HH:MM:SS` (`:122-129`;
  absolute because relative forms vary by systemd version, `:116-121`; test `tests/journalWarnings.test.ts:66-75`).
- Common args (`:240-249`): `--since <since> --grep <pattern> --case-sensitive=false --output=json --no-pager --lines=2000`.
- Unit args: `-u <basename(SINGBOX_UNIT_FILE_PATH,'.service')> -u <basename(REAR_SINGBOX_UNIT_FILE_PATH,'.service')> -u caddy`
  (defaults `sing-box`, `route-rear-singbox`, `caddy`) (`managedUnits`, `:218-224`). The mihomo rear unit
  (`route-rear-mihomo`) is **not** scanned.
- Two **sequential** passes (`:253-256`): (1) `journalctl <unitArgs> <common>`, fallbackSource `'unknown'`;
  (2) `journalctl -k <common>`, fallbackSource `'kernel'` (`-k` and `-u` are mutually exclusive, `:226-232`).
- execFile options: `maxBuffer: 8 MiB, timeout: 20 000 ms (PASS_TIMEOUT_MS), killSignal: 'SIGKILL'` (`:262-266`, rationale
  `:78-93`: grpc cancellation does not kill children; a 65 s scan vs 60 s RPC deadline incident; 2×20 s fits in 60 s).
- Error handling per pass (`:268-295`): if the rejection carries **non-empty stdout** → parse it anyway (covers exit 1
  with output, maxBuffer overflow, and a timeout kill that already produced output); else if `err.killed` → warn
  `'Journal warning scan pass hit the time limit — reporting without it'`; else if `err.code !== 1` → warn
  `'Journal warning scan pass failed'`; exit code 1 with empty stdout = "no matches", silent.
- `parseJournalJson(stdout, fallbackSource)` (`:143-177`): first 2000 lines; skip blank; skip non-JSON; skip if
  `MESSAGE` is not a string (byte-array messages for invalid UTF-8); classify (case-insensitive, `classifyLine` `:106-114`);
  skip unclassified; `source = _SYSTEMD_UNIT` without trailing `.service`, or fallbackSource if absent/non-string;
  `atUnixMs = floor(Number(__REALTIME_TIMESTAMP)/1000)` if finite and >0, else `Date.now()`; `sample = MESSAGE.slice(0, 300)` (JS chars).
- `aggregateWarnings` (`:187-215`): key `kind|source`; `occurrences` counts all entries (across both passes — the
  same line seen by both passes counts twice, test `tests/journalWarnings.test.ts:182-204`); sample and lastSeen from
  the entry with the greatest `atUnixMs` (ties → later entry wins, `>=`); output sorted by `lastSeenUnixMs` desc.

### 3.4 Listener check — `detectCoreNotListening()` (`src/utils/listenerWarning.ts:78-115`)
- Read `SINGBOX_CONFIG_PATH` (default `/etc/sing-box/config.json`); missing/unparseable → `null`.
- `publicListenPorts` (`:36-55`): inbounds whose `listen` is empty/absent, `0.0.0.0` or `::`; integer `listen_port > 0`; de-duplicated, config order.
  None → `null` (and `ss` is NOT invoked, test `tests/listenerWarning.test.ts:84-101`).
- shell `ss -lnu` and `ss -lnt` concurrently; either fails → `null` (debug log).
- `parseListeningPorts` (`:58-68`): per line regex `/[\s%][^\s]*:(\d+)\s+[^\s]+:\*/` → local port (any local
  address counts, including loopback); TCP and UDP results merged (protocol not distinguished).
- Missing = expected − listening. Empty → `null`. Else
  `{kind:'core_not_listening', source:'sing-box', sample:'конфиг объявляет порты ${expected.join(', ')}, но никто не слушает ${missing.join(', ')}', occurrences: missing.length, lastSeenUnixMs: Date.now()}`.
- Does not check whether sing-box is running/installed — only the config file (see Risks).

### 3.5 Rear rule-provider check — `detectEmptyRearRuleProviders()` (`src/utils/rearRuleProviderWarning.ts:73-113`)
- `readActiveRearClashApi()` (`src/utils/rearCore.ts:198-224`): active core = first existing config of
  [`REAR_MIHOMO_CONFIG_PATH` (`/etc/route-agent/rear.yaml`), `REAR_SINGBOX_CONFIG_PATH` (`/etc/route-agent/rear.json`)];
  parse with `JSON.parse` (mihomo YAML is written as JSON); address/secret: sing-box `experimental.clash_api.external_controller`/`secret`
  (`rearCore.ts:105-110`), mihomo top-level `external-controller`/`secret` (`:141-146`); address trimmed, empty → null.
  None → `null`.
- HTTP `GET http://<address>/providers/rules` with header `Authorization: Bearer <secret>` (**sent even when secret
  is empty**), `AbortSignal.timeout(3000)`. Non-2xx, network error, timeout or bad JSON → `null` silently.
- `emptyRuleProviders` (`:49-65`): for each `payload.providers[key]` object with **numeric** `ruleCount` ≤ 0 → name
  (`provider.name` if non-empty string else key); missing/non-numeric `ruleCount` is NOT empty; sorted ascending.
- Non-empty → warn log; `{kind:'rear_rule_provider_empty', source: core.label ('sing-box'|'mihomo'), sample:'наборы без правил: ${names.join(', ')}', occurrences: names.length, lastSeenUnixMs: Date.now()}`.

### 3.6 Orchestrator consumption
- Job `node-warnings` every `NODE_WARNING_POLL_INTERVAL_MS` = 15 min, first run at start (`orch:src/core/NodeWarningWorker.ts:379-387`,
  `orch:src/config/index.ts:259`). Only nodes online per `getAgentHealthStatus` are called (`:249-252`).
- RPC deadline = default **60 s** (`orch:src/core/services/grpc/GrpcConfigPusher.ts:62,535-551`), `onError:'returnFailure'`;
  UNIMPLEMENTED (old agent) → `success:false` → silently skipped (`:268-276`).
- Dedupe key `nodeId|kind|source`, re-notify window `NODE_WARNING_RENOTIFY_MS` = 12 h, committed only after
  Telegram send succeeds (`:337-372`, `orch:src/config/index.ts:265`). Titles per `kind` in `WARNING_TITLES`
  (`:51-92`); **unknown kinds are forwarded with the raw kind as title** — so kinds are a stable public contract.
  `occurrences > 1` shown as "повторов"; `lookbackHours` shown in text; `sample` shown verbatim; `lastSeenUnixMs` unused.

### 3.7 Parity checklist — PullNodeWarnings
1. Bad secret → OK `{success:false, message:'Invalid orchestrator secret token.', warnings:[], lookbackHours:0}`.
2. Exactly two `journalctl` invocations with the exact argv of §3.3, sequential, each SIGKILLed at 20 s, 8 MiB buffer cap.
3. `--since` uses local time; `--grep` is the `(?:a)|(?:b)…` join in registry order.
4. Exit code 1 + empty stdout = no matches, no log; non-zero exit with stdout = parse stdout.
5. Classification is case-insensitive, first-match-wins (`failed to increase receive buffer size` → `quic_buffer_not_increased`; the quic-go "sufficiently" line → `quic_recv_buffer`).
6. Source = `_SYSTEMD_UNIT` minus `.service`, else `unknown` (unit pass) / `kernel` (kernel pass).
7. µs→ms timestamp; missing timestamp → now; sample ≤300 chars; ≤2000 lines parsed per pass; non-string MESSAGE skipped.
8. Aggregation by `kind|source`, counts summed across passes, latest sample wins, sorted by lastSeen desc.
9. Output order: `core_not_listening`, `rear_rule_provider_empty`, journal classes.
10. Listener check silent when config missing/unparseable, no public inbounds (and `ss` not run), or `ss` fails.
11. Rear provider check silent when no rear config/endpoint, HTTP non-2xx/timeout(3 s)/bad JSON; empty = numeric `ruleCount ≤ 0`; names sorted.
12. `lookback_hours` = configured value on success and on internal error, 0 on auth failure.
13. Whole RPC must finish well under 60 s (worst case today ≈ 40 s + 3 s).

---

## 4. `RunNetworkDiagnostic` (unary)

Handler: `src/services/networkDiagnostic.service.ts:248-284`.

### 4.1 Inputs / validation
`NetworkDiagnosticRequest { orchestrator_secret, repeated string targets, repeated NetworkDiagnosticProbe probes }`.
- Auth failure → OK `{success:false, message:'Invalid orchestrator secret token.', results:[]}` (`:252-255`).
- `isValidDiagnosticTarget` (`:26-34`): string; trimmed length 1..253; not starting with `-`; regex
  `^[a-zA-Z0-9]([a-zA-Z0-9.:-]{0,252}[a-zA-Z0-9])?$` on the **trimmed** value. Test vectors `tests/app.test.ts:1580-1594`
  (`2001:db8::1` valid; spaces, `;`, `$()`, leading dash, 254 chars invalid).
- `normalizeProbe` (`:129-148`): `label = target` (or `String(target ?? '')`); `protocol = trim().toLowerCase()`;
  `port = Number(port) || 0`. Errors (checked in this order): invalid target → `'Invalid or unsafe target string.'`;
  protocol ∉ {icmp,tcp,udp} → `'Unsupported probe protocol: ${protocol || '(empty)'}.'`; tcp/udp with port ∉ [1,65535]
  → `'Probe port must be 1-65535 for ${protocol}.'`. Valid icmp → port forced 0. Target trimmed.

### 4.2 Execution
- All legacy `targets` (as icmp) and all `probes` run **concurrently** with no concurrency cap (`:270-273`, rationale
  `:262-269`: sequential runs blew the orchestrator's deadline). Result order: targets first, then probes, request order.
- Per probe: execFile `mtr` with `buildMtrArgs` (`:150-153`):
  `['--report','--json','--no-dns','--report-cycles','10', ...(icmp ? [] : ['--tcp'|'--udp','--port',String(port)]), target]`,
  `{ timeout: 45000 }` (default killSignal SIGTERM; default 1 MiB maxBuffer). Test `tests/networkDiagnosticProbes.test.ts:10-15`.
- `parseMtrJsonReport` (`:72-122`): JSON parse failure → throw `'Failed to parse mtr JSON output: <msg>'`; no
  `report.hubs` array or empty → throw `'mtr JSON report did not contain any hubs (hops).'`.
  - `lossyHops`: every hub with `Number(hub['Loss%']) > 0` → `{hopNumber: Number(count)||0, address: String(host ?? '???'), lossPercent, avgLatencyMs: Number(Avg)||0}`.
  - Last hub: `reachable = host.trim() non-empty && !== '???'`; `lossPercent = Number(last['Loss%'])||0`; `avgLatencyMs = Number(last.Avg)||0`.
  - `lastHopNumber/Address`: the last hub (scanning forward) whose host is non-empty, not `'???'`, and `Loss% < 100`; else 0/''.
    Tests `tests/networkDiagnosticProbes.test.ts:45-75`.
- Any mtr failure (ENOENT, timeout, non-zero exit, parse throw) → warn log, result with EMPTY values
  (`reachable:false, 0, 0, [], 0, ''`) and `error = err.message` (`:206-216`). Never throws.
- Invalid legacy target → result `{target: label, reachable:false, …, error:'Invalid or unsafe target string.', protocol:'icmp', port:0}` (`:218-225`).
- Invalid probe → result with the normalized (possibly bogus) `protocol` and `port` echoed and the error string (`:227-236`).
- Result object (`:176-198`): `target, reachable, lossPercent, avgLatencyMs, lossyHops[{hopNumber,address,lossPercent,avgLatencyMs}], error ('' on success), protocol, port, lastHopNumber, lastHopAddress`
  (snake_case twins in the TS object are dropped by the loader — irrelevant for Go). Valid probe echo: target trimmed, protocol lowercased, port 0 for icmp.
- Response: `success:true` always after auth, `message:'Network diagnostic completed for ${total} target(s): ${reachable} reachable, ${total−reachable} unreachable/error.'`, `results`.
  Empty request → success with 0 results (`tests/app.test.ts:1510-1517`).

### 4.3 Orchestrator consumption
- Client deadline **60 s** (`orch:src/core/services/grpc/GrpcNetworkDiagnosticClient.ts:30-37`); coupled to agent's
  45 s per-probe timeout and concurrency ("must be revisited together").
- Scheduled run: targets `NETWORK_DIAGNOSTIC_TARGETS` default `1.1.1.1,8.8.8.8` at `19:00,04:00`, nodes in chunks of 5;
  degraded if RPC failed, or any result `!reachable || lossPercent >= 20` (`orch:src/core/NetworkDiagnosticWorker.ts:75-94`,
  `orch:src/config/index.ts:320-326`); prints `lossyHops` hop/address/loss and `error`.
- Server reachability check: probes `icmp/0, tcp/22, tcp/443, udp/<MESH_AWG_PORT>` (`orch:src/shared/utils/serverReachability.ts:30-39`);
  results matched **by echoed `protocol` and `port`**, not position; any missing → agent considered outdated
  (`:75-91`). Uses `lastHopNumber/lastHopAddress` for "обрыв после хопа …"
  (`orch:src/zones/bot/commands/admin/screens/serverCheck.ts:77-78`, `orch:src/core/services/nodeOps/actions.ts:396`).

### 4.4 Parity checklist — RunNetworkDiagnostic
1. Bad secret → OK `{success:false, message:'Invalid orchestrator secret token.', results:[]}`.
2. Exact mtr argv per protocol; 45 s timeout per mtr; all probes concurrent; result order = targets then probes.
3. Target validation regex/length/leading-dash exactly as §4.1; probe validation order and error strings exactly.
4. Protocol/port echo: legacy targets `icmp/0`; probes echo normalized protocol (lowercase) and port (0 for icmp).
5. Parse rules for reachable / last-hub loss & latency / lossyHops (>0 only) / last answering hop (<100 % loss, not `???`).
6. Per-probe failure never fails the RPC; `success` is always true post-auth; `error` empty on success.
7. Message format exactly `Network diagnostic completed for N target(s): R reachable, U unreachable/error.`

---

## 5. `GetWarpKeyHealth` (unary)

Handler: `src/services/warpKeyHealth.service.ts:26-84`; parser `src/utils/warpKeyHealth.ts`.

### 5.1 Flow
1. Auth failure → OK `{success:false, message:'Invalid orchestrator secret token.', entries:[]}` (`:30-33`; no skippedReason).
2. Gate: `fs.stat(config.REAR_SINGBOX_CONFIG_PATH || '/etc/sing-box/rear.json')` (zod default makes it
   `/etc/route-agent/rear.json`, `src/config.ts:35`). Missing → `{success:true, message:'Rear sing-box is not configured on this node.', entries:[], skippedReason:'rear_not_running'}` (`:35-46`).
3. `readActiveRearClashApi()` (§3.5 — note it prefers the **mihomo** config if present). `null` →
   `{success:true, message:'Rear sing-box config carries no clash_api endpoint.', entries:[], skippedReason:'rear_not_running'}` (`:53-61`).
4. `fetchWarpKeyHealth(endpoint)` (`src/utils/warpKeyHealth.ts:85-99`): `GET http://<address>/proxies`,
   header `authorization: Bearer <secret>` **only if secret non-empty**, `AbortSignal.timeout(5000)`; non-2xx → throw
   `'clash api responded <status> for /proxies'`; JSON parse.
5. `parseWarpKeyHealth(payload)` (`warpKeyHealth.ts:63-83`): `payload.proxies` must be an object (else `[]`); for each
   `[tag, entry]` in object key order: keep only `tag.startsWith('warp-')` with a non-empty remainder, excluding
   `'warp-auto'` (mihomo group, `:40-47`); `parseHistory(entry.history)` (`:49-61`): non-empty array → last element;
   `delay` must be a finite number > 0 else "not measured"; `time` string → `Date.parse`, unparseable → 0.
   Record `{endpointTag: tag, alive: measured!==null, rttMs: delay ?? 0, measuredAtUnixMs: at ?? 0}`.
   "ПУСТАЯ ИСТОРИЯ ЗНАЧИТ «НЕ ОТВЕТИЛ»" — sing-box deletes history on a failed probe (`:15-18`). Tests `tests/warpKeyHealth.test.ts:63-158`.
6. Success → `{success:true, message:'Reported health for ${n} WARP endpoint(s).', entries}` (skippedReason unset = `''`).
7. Any throw in 2-6 → warn log; `{success:false, message:'Could not read rear clash api: <msg>', entries:[], skippedReason:'clash_api_unreachable'}` (`:74-83`).

No measurement is performed by the agent (`proto/agent.proto:202-216`).

### 5.2 Orchestrator consumption (`orch:src/core/WarpHealthWorker.ts`)
- Every `WARP_HEALTH_PROBE_INTERVAL_MS` = 1 h (`orch:src/config/index.ts:224`), WARP-enabled nodes that are online;
  deadline default 60 s.
- `!success` → warn, skip; `skippedReason` non-empty (any value) → debug, skip (`:97-107`).
- `keyIdFromEndpointTag`: strip `warp-` prefix (`orch:src/core/generators/rearSingbox.ts:134-136`), match to DB keys of
  that node; record `alive` and `rttMs` (only when alive); 2nd consecutive `alive:false` → key swapped and rear
  config re-pushed (`:124-171`). `measuredAtUnixMs` only logged. ⇒ `alive` semantics are load-bearing (a false
  "dead" burns WARP keys).

### 5.3 Parity checklist — GetWarpKeyHealth
1. Bad secret → OK `{success:false, message:'Invalid orchestrator secret token.', entries:[]}`, empty skippedReason.
2. No `REAR_SINGBOX_CONFIG_PATH` file → `success:true`, `skippedReason:'rear_not_running'` (exact message).
3. Config present but no clash endpoint (or unparseable) → `success:true`, `rear_not_running`, second message.
4. Endpoint unreachable / non-2xx / timeout 5 s / bad JSON → `success:false`, `clash_api_unreachable`, `Could not read rear clash api: …`.
5. Tag filter: `warp-<non-empty>` except `warp-auto`; `warp`, `direct`, `GLOBAL`, `warp-` excluded.
6. Empty/absent history or `delay ≤ 0` → `alive:false, rttMs:0, measuredAtUnixMs:0`; latest history entry used; bad time → 0 but alive kept.
7. Authorization header omitted when secret is empty.

---

## 6. Background timers / loops in scope

| Timer | Where | Period | Notes |
|---|---|---|---|
| AWG health/self-heal | `src/utils/telemetry.ts:603-614`, started `src/index.ts:123` | 30 s `setInterval` | disabled under `NODE_ENV=test`; never stopped |
| Telemetry tick | `src/services/telemetry.service.ts:141` | 2 s per open stream | cleared on cleanup |
| `journalctl -f` child | `telemetry.service.ts:40` | lifetime of stream | SIGTERM on cleanup |
| Version/kernel caches | `telemetry.ts:146-157,307-329,378-389` | 60 s TTL (lazy) | no background refresh |

(Adjacent but out of scope: rear WARP guard every `REAR_WARP_GUARD_INTERVAL_MS`=30 s, `src/index.ts:215`, which reads the same Clash API.)

---

## 7. Non-obvious "why" notes (quotes)

- Unit-not-binary for sing-box installed signal: "ОТВЕЧАЕТ НЕ «ЕСТЬ ЛИ ФАЙЛ», А «РАБОТОСПОСОБНО ЛИ ЯДРО»" (`src/utils/telemetry.ts:101`).
- Version suffix kept: "`1.14.0-hotusers` приезжало в оркестратор как `1.14.0` … Суффикс здесь не украшение" (`telemetry.ts:118-123`).
- Mesh not auto-healed: "silently self-healing it could mask a real connectivity problem" (`telemetry.ts:344-347`).
- Live mesh keys needed per peer: "у эстонского узла из четырёх фронтов рукопожатие было с одним" (`telemetry.ts:402-411`).
- Host traffic on default-route iface only: "Сумма всех интерфейсов задвоила бы мосты, veth и туннель меша" (`src/utils/hostNetwork.ts:8-13`).
- Journal stateless by design: "агент НЕ помнит, о чём уже докладывал … Дедупликация — забота оркестратора" (`src/utils/journalWarnings.ts:20-23`).
- Silence ≠ health for QUIC buffers: root with `CAP_NET_ADMIN` uses `SO_RCVBUFFORCE` (`journalWarnings.ts:43-48`).
- Pass timeout + SIGKILL: grpc deadline doesn't kill children, 65 s scan vs 60 s deadline (`journalWarnings.ts:78-93`).
- Listener check: "Нода, три недели простоявшая без единого слушателя на публичном порту, не написала об этом ни строчки" (`src/services/nodeWarnings.service.ts:38-42`).
- Empty rule provider: germany-node, `ip-telegram` `ruleCount:0`, MTProto went DIRECT (`src/utils/rearRuleProviderWarning.ts:9-19`).
- Diagnostic concurrency: sequential mtr runs exceeded the orchestrator deadline (`src/services/networkDiagnostic.service.ts:262-269`).
- `success` in RunNetworkDiagnostic means "attempted all targets", not "all reachable" (`networkDiagnostic.service.ts:238-247`).
- Three WARP outcomes deliberately distinct (`src/services/warpKeyHealth.service.ts:11-25`).

---

## 8. Risks / smells (flag, don't fix)

1. **No timeouts on any per-tick exec** (`awg show`, `systemctl`, `ip`, `modinfo`, `lsmod`, `--version` probes) and
   `setInterval` doesn't wait → a hung command stacks overlapping ticks and child processes every 2 s per stream; a
   slow first tick can miss the orchestrator's **5 s first-frame watchdog**. Go port: per-command context timeouts
   and a non-overlapping ticker.
2. **Telemetry reads trigger side effects**: every frame / GetTelemetry runs `checkAndHealAwgInterface`, which can
   `systemctl restart route-awg@awg0` *inside* the tick (blocking the frame). The restart decision is check-then-act
   across an `await` on shared state → concurrent streams + the 30 s timer can restart twice. A failed restart sets
   no cooldown → retried every 2 s per stream.
3. **GetWarpKeyHealth is gated on the sing-box rear config only** (`warpKeyHealth.service.ts:35-46`), while
   switching to mihomo deletes that file (`src/services/rearSingbox.service.ts:254-255`). ⇒ on mihomo-rear nodes the
   RPC always says `rear_not_running`, so WARP key health is never reported even though the parser explicitly
   handles mihomo (`warp-auto`). Likely a bug; the owner must decide whether Go preserves it. Also, if both configs
   exist, the gate checks sing-box but the read uses mihomo's endpoint.
4. **Unary GetTelemetry auth failure returns OK** with a sentinel body; the orchestrator caches it as live telemetry
   and marks the node online (`orch:…/GrpcTelemetryStreamManager.ts:954-959`), with singbox shown "not installed".
   The stream path, by contrast, returns UNAUTHENTICATED. Inconsistent.
5. `awg_active_peers` hard-codes `awg0` while the health check uses `getAwgInterfaceName()`; health check's
   "configured" test uses `AWG_CONFIG_PATH` regardless of the `iface` argument.
6. `system_logs` and `awg_status` are unused by the orchestrator, yet `system_logs` costs a long-lived `journalctl -f`
   per stream; `journalctl -u sing-box` hard-codes the unit name.
7. CPU math: sums guest/guest_nice (double count), iowait counted as busy, unary/first frame = since-boot average.
8. `active_connections` ignores IPv6 and counts LISTEN/TIME_WAIT; reading `/proc/net/tcp` on a busy node is O(sockets) every 2 s per stream.
9. `call.write` backpressure ignored → unbounded buffering if the client stops reading. Cancel during the first
   awaited tick leaks the subsequently-created interval (it never clears; ticks no-op).
10. Shared `lastAwgCheckTime` between tools and go version caches; no in-flight dedupe of cold-cache probes across streams.
11. Proto comments are wrong about value sets (`mesh_awg_kernel_status` has `built_not_loaded`, never `error` on the
    success path; `mesh_tunnel_interface_status` never `down`). Orchestrator treats `down` as handled anyway.
12. Listener check doesn't verify sing-box is active/installed: a stale `config.json` after unit removal yields a
    false `core_not_listening`. It also treats any-address listeners (incl. 127.0.0.1) as satisfying a public port,
    and merges TCP/UDP.
13. Journal scan ignores the mihomo rear unit (`route-rear-mihomo`). `-u` also matches systemd's own messages about the
    unit (their `_SYSTEMD_UNIT` is typically `init.scope`, which would become the `source`).
14. `sinceArgument` relies on the agent's local TZ matching journalctl's (same process env → consistent; keep `time.Local`).
15. Rear-provider check sends `Authorization: Bearer ` with an empty secret; WARP health omits it — inconsistent.
16. RunNetworkDiagnostic has no cap on number of concurrent `mtr` processes (request-controlled fan-out).
17. `Date.parse` of Go RFC3339Nano times (9 fractional digits) — V8 behavior should be confirmed; Go `time.Parse(RFC3339Nano)` truncated to ms is the natural equivalent.

### 8.1 Shell-outs replaceable by procfs / netlink / D-Bus

| Current | Replacement | Data that must be reproduced |
|---|---|---|
| `ip link show <iface>` (awg0, awgmesh0) | `stat /sys/class/net/<iface>` or rtnetlink `RTM_GETLINK` | existence only (exit 0 ⇔ link exists) |
| `awg show <iface> dump` | WireGuard-compatible control API: kernel module (mesh) via generic netlink (AmneziaWG family — verify name, likely `amneziawg`); userspace `amneziawg-go` (client awg0) via its UAPI unix socket (`get=1`, verify path, typically `/var/run/amneziawg/<iface>.sock`) | per peer: public key (base64, as printed), latest handshake unix seconds (0 = never) |
| `lsmod \| grep -w amneziawg` | `/proc/modules`, first column == `amneziawg` (or `/sys/module/amneziawg/initstate == live`) | loaded yes/no |
| `modinfo amneziawg` | resolve via `/lib/modules/$(uname -r)/modules.dep`, read the `.modinfo` ELF section of the `.ko` (`version=`); simpler to keep the exec (60 s cache) | presence for the running kernel + `version` string |
| `systemctl list-units --state=running 'olcrtc-agent-srv@*'` | systemd D-Bus `ListUnitsByPatterns(["running"], ["olcrtc-agent-srv@*"])` | count of running instances |
| `systemctl is-enabled route-awg@awg0` | D-Bus `GetUnitFileState("route-awg@awg0.service") == "enabled"` | exact `enabled` only (not `enabled-runtime`, `static`, …) |
| `ss -lnt` / `ss -lnu` | `/proc/net/{tcp,tcp6}` state `0A` and `/proc/net/{udp,udp6}` (unconnected, rem `:0000`) local ports, or `NETLINK_SOCK_DIAG` | set of local listening ports, any address, v4+v6, TCP∪UDP |
| `journalctl -u sing-box -f` | sd-journal API (cgo) or keep the child | tail of unit messages, plain text (`--output cat`) |
| `journalctl --grep … --output=json` | sd-journal API with matches `_SYSTEMD_UNIT=` / `_TRANSPORT=kernel` + local regex; keep the exec for parity of `-u` semantics | MESSAGE, `_SYSTEMD_UNIT`, `__REALTIME_TIMESTAMP` |
| `/proc/net/tcp` + `/proc/net/udp` line counts | `NETLINK_SOCK_DIAG` counts, or `/proc/net/sockstat` (**different semantics** — sockstat `inuse` excludes TIME_WAIT) | for parity: count of IPv4 TCP sockets in all states + IPv4 UDP sockets |

`/proc/stat`, `/proc/meminfo`, `/proc/net/route`, `/proc/net/dev` are already file reads.

---

## 9. Undetermined / to verify on a live node
- Exact AmneziaWG generic-netlink family name and amneziawg-go UAPI socket path (needed only if replacing `awg show`).
- Whether mihomo's `/proxies` history format matches sing-box's (`time`, `delay`) — moot while risk §8.3 holds.
- V8 `Date.parse` handling of 9-digit fractional seconds in sing-box history times.
