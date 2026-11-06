# Spec D — networking: AWG, mesh tunnel, firewall, traffic shaping, kernel tuning

Status: behavioural spec of the TypeScript agent (`route-agent`, branch `go-rewrite`) for a zero-loss Go port.
Scope: RPCs `ConfigureAwg`, `GetAwgObservation`, `ConfigureMeshTunnel`, `ManageFirewall`, `ApplyTrafficThrottle`,
plus `UploadMeshAwgKernelSource` (the DKMS build that the mesh depends on), and the modules
`utils/awg.ts`, `utils/awgFlows.ts`, `services/awgObservation.service.ts`, `services/meshTunnel.service.ts`,
`utils/kernelTuning.ts`, `services/firewall.service.ts`, `utils/firewall.ts`, `utils/trafficShaper.ts`,
`services/trafficThrottle.service.ts`, `services/systemdUnit.service.ts` (the `route-awg@.service` unit),
and the AWG/mesh parts of `utils/telemetry.ts` (health timer, mesh status) that feed `TelemetryResponse`.

All paths below are relative to `/home/user/route-agent` unless prefixed with `orch:` (= `/home/user/vpn-orchestrate`).

---

## 0. Cross-cutting facts the Go port must reproduce

### 0.1 Proto loading semantics (src/index.ts:51-57)

The server loads `proto/agent.proto` with `keepCase: false, longs: String, enums: String, defaults: true, oneofs: true`.
Consequences that change observable behaviour:

- **`defaults: true`** — every absent scalar arrives as its proto3 zero value (`0`, `""`, `false`, `[]`), never `undefined`.
  So checks like `jc !== undefined && jc !== null` (src/services/config.service.ts:604-606, src/services/meshTunnel.service.ts:427-429)
  are **always true**: `Jc = 0`, `Jmin = 0`, `Jmax = 0` are written whenever the orchestrator omits them. Go's proto3
  zero values give exactly this for free — keep the "always write Jc/Jmin/Jmax" behaviour.
- `enabled` absent ⇒ `false` ⇒ `ConfigureAwg` takes the **disable** path.
- Snake-case fallbacks in handlers (`open_udp_ports`, `allowed_ips`, `persistent_keepalive`, `orchestrator_secret`) are dead
  code under `keepCase:false`; they never fire. In Go just read the generated fields.
- `longs: String` affects only the server's *request* decoding; response int64 fields are emitted from JS numbers. The
  orchestrator decodes int64 as strings (`orch:src/core/services/grpc/GrpcConnectionManager.ts:18-19`) and normalises via
  `grpcInt64` (`orch:src/zones/anomaly-traffic/services/throttleDelivery.ts:184-194`). Go sends native int64 — compatible.

### 0.2 Authentication (src/middleware/auth.ts)

- `verifySecret(token)` (auth.ts:7-19): false if token empty/non-string, or `EGRESS_CONTROL_SECRET` empty, or byte lengths
  differ; else `crypto.timingSafeEqual` on UTF-8 bytes. Go: `subtle.ConstantTimeCompare` after explicit length/empty checks.
- `extractSecretFromMetadata(call)` (auth.ts:23-26): first value of metadata key `x-orchestrator-secret`, stringified, else `""`.
- `authenticateCall(call)` (auth.ts:31-38): metadata secret OR `request.orchestratorSecret` (payload field).

Which RPC uses what:

| RPC | Auth | Payload secret honoured? |
|---|---|---|
| ConfigureAwg | metadata only (config.service.ts:501-505) | n/a (no field in proto) |
| ConfigureMeshTunnel | metadata only (meshTunnel.service.ts:383-387) | **No**, although proto has `orchestrator_secret = 1` |
| ManageFirewall | metadata only (firewall.service.ts:15-19) | n/a |
| GetAwgObservation | `authenticateCall` (awgObservation.service.ts:23) | yes |
| ApplyTrafficThrottle | `authenticateCall` (trafficThrottle.service.ts:22) | yes |
| UploadMeshAwgKernelSource | metadata, else per-chunk `orchestratorSecret` until verified (meshTunnel.service.ts:274-306) | yes |

Unauthorized responses are **gRPC OK** with `{success:false, message:'Invalid orchestrator secret token.'}` plus
zero/empty extra fields (exact per RPC below). The literal `'Invalid orchestrator secret token.'` is asserted by
tests (tests/app.test.ts:1053, 1191) — keep it byte-exact.

The orchestrator always sends the secret in metadata (`orch:src/core/services/grpc/GrpcConnectionManager.ts:329-333`).
Transport is mandatory mTLS (src/index.ts:69-116, out of this spec's scope).

### 0.3 How commands are executed (src/utils/exec.ts:1-5)

- `execAsync = promisify(child_process.exec)` → runs **`/bin/sh -c "<string>"`** (on Debian/Ubuntu `/bin/sh` = dash).
  Defaults: no timeout unless given, `maxBuffer` = 1 MiB (stdout or stderr overflow ⇒ child killed, error
  `ERR_CHILD_PROCESS_STDIO_MAXBUFFER`), `killSignal` SIGTERM (kills only the `sh`, not grandchildren).
- `execFileAsync = promisify(child_process.execFile)` → no shell, argv exactly as given, PATH lookup.
- Error shape (verified in this sandbox): `err.message = "Command failed: " + <cmd string or file+' '+args.join(' ')> + "\n" + <stderr>`;
  `err.stderr`, `err.stdout`, `err.code` (exit code), `err.killed`/`err.signal` (timeout ⇒ `killed=true`, `signal='SIGTERM'`).
  **The orchestrator parses this format**: `orch:src/shared/utils/failureDigest.ts:39-55` takes line 1 as "what was done"
  and then greps the rest for compiler errors, `^Error! ` (dkms), `^dpkg: error processing package `, `^E: `, plus the
  `Autoinstall of module X for kernel Y` hint. The Go port must build error messages as
  `"Command failed: <command>\n<stderr>"` wherever the TS code surfaces `err.message`.
- Several call sites prefer `(err.stderr || err.stdout || err.message).trim()` — noted per call.

### 0.4 `NODE_ENV === 'test'` short-circuits

Many side effects are skipped under `NODE_ENV=test`. The Go port needs an equivalent dry-run seam (interface for
exec/fs) so the same tests can be ported; the list of skipped steps is given per handler.

### 0.5 Global state / concurrency in this area

No mutex anywhere in scope. grpc-js runs handlers concurrently (interleaving at every `await`). Shared mutable state:

| State | Where | Lifetime |
|---|---|---|
| `awgHealthStates: Record<iface, AwgHealthState>` | src/utils/telemetry.ts:470 | process |
| `awgHealthCheckTimer` | src/utils/telemetry.ts:601 | process |
| `cachedMeshAwgKernelStatus` + `lastMeshAwgKernelCheckTime` (TTL 60 s) | src/utils/telemetry.ts:378-394 | process |
| `/etc/route-agent/traffic-throttle.json` | src/utils/trafficShaper.ts:38 | disk |
| `/opt/route-agent/active_ports.json` | src/utils/firewall.ts:12 | disk |
| AWG/mesh config files, sysctl profile, unit file | see below | disk |

---

## 1. RPC `ConfigureAwg` — client-facing AmneziaWG interface

Handler: `configureAwgHandler`, src/services/config.service.ts:497-730. Registered at src/index.ts:163.
Orchestrator caller: `orch:src/core/services/grpc/GrpcConfigPusher.ts:631-653` (deadline **60 s**, `onError:'returnFailure'`,
gRPC errors become `"gRPC Failure [Code N]: msg"`); payload built by
`orch:src/core/services/orchestrator/awgOrchestrator.ts:568-608`; result consumed by `pushAwgConfigToNode`
(awgOrchestrator.ts:690-735): only `success !== false` and `message` matter.

### 1.1 Inputs actually read (proto `AwgConfigPayload`, proto/agent.proto:390-425)

`enabled, port, serverPrivateKey, serverPublicKey (destructured, never used), addressV4, addressV6, jc, jmin, jmax,
s1..s4, h1..h4, i1..i5, headerProtectionKey, peers[{publicKey, presharedKey, allowedIps}], ipv6Mode, randomTrailers, disableCookies`
(config.service.ts:507-535).

What the orchestrator sends (awgOrchestrator.ts:596-607): `enabled:true`, `port` (node setting or 51820),
`addressV4:'100.64.0.1/16'` (hard-coded), `addressV6:'<subnet-prefix>::1/<mask>'`, mimicry fields (numeric parsed with
`parseUint`, may be `undefined` ⇒ wire 0), `ipv6Mode` default `'trap-ipv6'`, flags `'on'|'off'|''`,
peers with `allowedIps` `"<v4>/32"` or `"<v4>/32, <v6>/128"` (awgOrchestrator.ts:552-559). There is no disable caller found
in the orchestrator's main push path (it always sends `enabled:true`); `systemOrchestrator.configureAwgOnNode`
(`orch:src/core/services/orchestrator/systemOrchestrator.ts:1007-1011`) is a passthrough that could send `enabled:false`.

### 1.2 Sanitisation / validation

- `sanitizeConfigInput(v)` (config.service.ts:27-30): `undefined|null → ''`, else `String(v)` with every `\r` and `\n`
  removed, then `.trim()`. Applied to privkey, addresses, HPK, I1-I5, peers' fields, and S/H **after** the raw check.
- S1-S4/H1-H4: written iff raw `!== undefined && !== null && !== ''`; value = sanitized. A raw `"  "` passes the check and
  writes `S1 = ` (empty value). I1-I5 and HPK are sanitized first and omitted when empty (subtle difference — keep it).
- `awgBoolDirective(name, raw)` (config.service.ts:38-43): only `typeof raw === 'string'`; `trim().toLowerCase()`;
  `'on'|'1' → "<name> = on\n"`, `'off'|'0' → "<name> = off\n"`, anything else (incl. `'on\nPostUp…'`) → `''`.
  Tests: tests/awgBoolDirective.test.ts:5-21.
- No validation of port range, key format, CIDR, ipv6Mode values. `port` is uint32; `0` is falsy.

### 1.3 Disable path (`!enabled`), config.service.ts:542-564

Skipped entirely under NODE_ENV=test (still returns success). In order:
1. `execAsync("systemctl stop route-awg@<iface> || systemctl stop awg-quick@<iface> || awg-quick down <iface> || true")`
   (shell, no timeout). Error → warn only.
2. If `port` truthy AND `isUfwInstalled()` (`sh -c 'command -v ufw'`, firewall.ts:23-30):
   `execAsync("sudo ufw delete allow <port>/udp || true")` then `execAsync("sudo ufw reload || true")`. Errors → warn.
3. Return `{success:true, message:'AmneziaWG (AWG3) service successfully disabled and stopped.'}`
   (test asserts substring `disabled and stopped`, tests/app.test.ts:1128-1138).

NOT done on disable: config file is not removed, unit is not `systemctl disable`d, iptables rules stay. See Risk R3.

`<iface>` = `getAwgInterfaceName()` (src/utils/awg.ts:12-16): `basename(config.AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/awg0.conf', '.conf') || 'awg0'`.

### 1.4 Enable path — side effects in order (config.service.ts:566-724)

Steps marked (T) are skipped under NODE_ENV=test.

1. (T) `execAsync('sysctl -w net.ipv4.ip_forward=1')` — error → warn. **Not persisted** to any file.
2. (T) if `ipv6Mode === 'dual-stack' || ipv6Mode === 'trap-ipv6'`: `execAsync('sysctl -w net.ipv6.conf.all.forwarding=1')`; error → debug.
3. Build config text (exact format, `\n` line endings, no trailing blank line beyond last `\n`):
   ```
   [Interface]
   PrivateKey = <priv>             (if non-empty)
   ListenPort = <port>             (if port truthy, i.e. != 0)
   Address = <v4>[, <v6>]          (v4 if non-empty; v6 if non-empty AND ipv6Mode !== 'ipv4-only'; joined by ", "; line only if ≥1)
   Jc = <jc>                       (always, see 0.1)
   Jmin = <jmin>                   (always)
   Jmax = <jmax>                   (always)
   S1 = … S4 = …                   (raw non-empty)
   H1 = … H4 = …                   (raw non-empty)
   I1 = … I5 = …                   (sanitized non-empty)
   HeaderProtectionKey = <hpk>     (sanitized non-empty)
   RandomTrailers = on|off         (awgBoolDirective)
   DisableCookies = on|off         (awgBoolDirective)
   ```
   then for each peer (skip null; skip if sanitized publicKey empty):
   ```

   [Peer]
   PublicKey = <pk>
   PresharedKey = <psk>      (if non-empty)
   AllowedIPs = <allowedIps> (if non-empty; passed verbatim incl. ", " separators)
   ```
   (config.service.ts:599-637). No Endpoint/PersistentKeepalive for client peers. `serverPublicKey` is ignored.
4. `fs.mkdir(dirname(AWG_CONFIG_PATH), {recursive:true})` (default mode 0777&~umask), then
   `fs.writeFile(AWG_CONFIG_PATH, text, 'utf-8')` — **in place, not atomic, default mode 0666&~umask (=0644 under umask 022)**,
   owner root (agent runs as root, install.sh:286). Default path `/etc/amnezia/amneziawg/awg0.conf` (src/config.ts:81).
   Errors here throw → outer catch (step 11).
5. (T) `execAsync('ip link show <iface>')` → success ⇒ `interfaceExists`.
6. (T) If exists: `execAsync('systemctl reload route-awg@<iface>')`. On error: `reloadErr = (stderr||stdout||message||'systemctl reload error').trim()`
   logged warn, then `execAsync('systemctl restart route-awg@<iface>')`; on that error return
   `{success:false, message:'Failed to reload or restart AWG service (route-awg@<iface>): <restartMsg>'}` where
   `restartMsg = (stderr||stdout||message||'systemctl restart error').trim()`.
   If not exists: `execAsync('systemctl start route-awg@<iface>')`; error → `{success:false, message:'Failed to start AWG service (route-awg@<iface>): <startMsg>'}`.
   **There is no "identity changed" detection here** (unlike mesh): Address/MTU changes rely on reload failing (see R1).
7. (T) `execAsync('systemctl enable route-awg@<iface>')`; error → warn.
8. (T) NAT/forward (errors → warn, never fail the RPC), **interface name hard-coded `awg0`**:
   - `execAsync('iptables -C FORWARD -i awg0 -j ACCEPT || iptables -A FORWARD -i awg0 -j ACCEPT')`
   - `execAsync('ip route show default')`; `defaultIface` = first `/dev\s+([^\s]+)/` match; if found:
     `execAsync('iptables -t nat -C POSTROUTING -o <defaultIface> -j MASQUERADE || iptables -t nat -A POSTROUTING -o <defaultIface> -j MASQUERADE')`
     (inner errors swallowed silently, config.service.ts:691-698).
9. (T) if `ipv6Mode === 'trap-ipv6'`:
   `execAsync('ip6tables -C FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited || ip6tables -A FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited')`;
   error → debug. (No IPv6 NAT is ever installed for `dual-stack`.)
10. (T) if `port` truthy and ufw installed: `execAsync('sudo ufw allow <port>/udp')` then `execAsync('sudo ufw reload')` (unconditional reload; error → warn).
11. Return `{success:true, message:'AmneziaWG (AWG3) interface awg0 configured and active on port <port || 51820>.'}` —
    literal `awg0` even if the iface differs; `51820` printed when port=0 although no ListenPort was written.
    Outer catch (config.service.ts:725-729): `{success:false, message:'AmneziaWG configuration error: <err.message>'}`.

No timeouts on any of these commands; the orchestrator gives up at 60 s but the agent keeps going.
A healthy push with 30 peers took 9.5 s (orch:GrpcConfigPusher.ts:57-58).

### 1.5 Idempotency

Re-sending the same payload rewrites the file (always), reloads/restarts the unit (always), re-enables, `iptables -C … || -A`
is idempotent, `ufw allow` is idempotent ("Skipping adding existing rule", firewall.ts:40-41) but `ufw reload` runs every time.

---

## 2. The `route-awg@.service` template unit (shared by AWG and mesh)

`ensureAwgSystemdUnit(overridePath?)`, src/services/systemdUnit.service.ts:14-62, called once, awaited, at startup before
the gRPC server binds (src/index.ts:120).

- Path: `overridePath || config.AWG_UNIT_FILE_PATH || '/etc/systemd/system/route-awg@.service'`.
- `awgBin = config.AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg'`; `awgQuickBin = awgBin + '-quick'`
  (**not** `AWG_QUICK_BINARY_PATH`, which binary.service.ts:455 uses for uploads — they coincide only with defaults).
- Exact content with defaults (byte-exact, trailing `\n`):
  ```
  [Unit]
  Description=AmneziaWG interface %i (managed by route-agent)
  After=network-online.target
  Wants=network-online.target

  [Service]
  Type=oneshot
  RemainAfterExit=yes
  ExecStart=/usr/local/bin/awg-quick up %i
  ExecStop=/usr/local/bin/awg-quick down %i
  ExecReload=/bin/sh -c '/usr/local/bin/awg syncconf %i <(/usr/local/bin/awg-quick strip %i)'

  [Install]
  WantedBy=multi-user.target
  ```
- If existing content is byte-identical → return false, no write, no daemon-reload. Else mkdir -p, writeFile (in place),
  then (T) `execAsync('systemctl daemon-reload')` (error → warn); return true. Any other error → logged, return false
  (startup continues). Test: tests/app.test.ts:536-556.
- **The `ExecReload` uses `/bin/sh` with bash process substitution `<(…)`** — see Risk R1.

`AWG_RELOAD_COMMAND` (src/config.ts:82, default `systemctl restart route-awg@awg0`) is declared but unused anywhere.

---

## 3. `restartActiveAwgServices()` (src/utils/awg.ts:38-91)

Not an RPC; called after `UploadAwgToolsBinary` and `UploadAwgGoBinary` installs (src/services/binary.service.ts:501, 608, under NODE_ENV≠test).

1. `execAsync("systemctl list-units 'route-awg@*' 'awg-quick@*' --state=active --plain --no-legend")`; for each trimmed
   non-empty line take regex `^(route-awg@[^\s]+|awg-quick@[^\s]+)` group 1 into a Set (errors ignored).
2. If empty: `execAsync('systemctl is-active route-awg@<primaryIface>')`; add if stdout.trim() === 'active' (errors ignored; note
   `is-active` exits non-zero when inactive).
3. If still empty: log info `AWG interface <iface> not yet configured, skipping restart — will use new binary on first configure`,
   return `{reloaded:false, restartedUnits:[]}`.
4. Sequentially `execAsync('systemctl restart <unit>')` for each (Set insertion order); failures warn and are skipped.
5. Return `{reloaded: restartedUnits.length>0, restartedUnits}`. Caller message suffix:
   `' and active AWG service(s) restarted'` / `' (service restart skipped: not yet configured)'` (binary.service.ts:502-506).

Note: this also restarts `route-awg@awgmesh0` (mesh) and any foreign `awg-quick@*` unit. Test: tests/app.test.ts:755-762.

---

## 4. AWG health check / self-healing (background) — src/utils/telemetry.ts:463-629

### 4.1 Triggers
- `startAwgHealthCheckTimer(intervalMs = 30000)` (telemetry.ts:603-614), called at startup (src/index.ts:123); no-op if
  already started or NODE_ENV=test. `setInterval(async …)` — ticks can overlap; errors → debug.
- **Also** `getAwgStatus()` (telemetry.ts:623-629) calls `checkAndHealAwgInterface()` on **every telemetry tick**
  (StreamTelemetry every 2000 ms per open stream, src/services/telemetry.service.ts:97,141; and every GetTelemetry,
  telemetry.service.ts:185). So the effective check cadence is ~2 s while the orchestrator streams.

### 4.2 `checkAndHealAwgInterface(iface = getAwgInterfaceName())`
1. State default `{lastCheckTime:0, lastAutoRestartTime:0, autoRestartCount:0, status:'unknown'}`.
2. Test hook: `TEST_AWG_STATUS` set ⇒ status = it, return.
3. Configured? `fs.stat(config.AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/<iface>.conf')` exists OR
   `execAsync('systemctl is-enabled route-awg@<iface>')` stdout.trim()==='enabled'. (Test: `TEST_AWG_UNCONFIGURED !== 'true'`.)
   Not configured ⇒ status `'inactive'`, return.
4. Alive? `execAsync('ip link show <iface>')` succeeds (test: `TEST_AWG_ALIVE !== 'false'`).
5. If not alive: cooldown `RESTART_COOLDOWN_MS = 300000`. If `lastAutoRestartTime === 0 || now-last >= 300000`:
   `execAsync('systemctl restart route-awg@<iface>')`; **only on success** set `lastAutoRestartTime=now`, `autoRestartCount++`
   (failure → error log, no cooldown recorded ⇒ retried on the next check, i.e. every ~2 s). Else warn with `waitSec`.
6. Status string (telemetry.ts:560-585):
   - `restartInfo` if `lastAutoRestartTime>0`: `restarted <m>m ago` when elapsed < 60 min (m = floor minutes, ≥0) else `restarted <h>h ago`.
   - alive: `nominal (<restartInfo>)` if restartInfo and elapsed < 24 h, else `nominal`.
   - not alive: `crashed (<restartInfo>)` if restartInfo else `crashed`.
7. `lastCheckTime=now`; store; return.

Test: tests/app.test.ts:764-793 (nominal → crashed (restarted…) count 1 → still 1 within cooldown → `nominal (restarted…`).
`awgStatus` goes into `TelemetryResponse.awg_status` (telemetry.service.ts:118); the orchestrator does not branch on it
(only typed in `orch:src/types/grpc.ts`).

### 4.3 Deliberately NOT self-healed: the mesh interface
telemetry.ts:343-347: "Deliberately NOT wired into checkAndHealAwgInterface's auto-restart loop … silently self-healing it
could mask a real connectivity problem an admin needs to see". Go port must not add mesh self-heal.

### 4.4 Telemetry readers in scope
- `getAwgActivePeersCount()` (telemetry.ts:167-200): `execAsync('awg show awg0 dump')` (**hard-coded awg0**); lines after the
  first; `split(/\s+/)`, ≥5 parts, `parts[4]` parsed int, count if `>0 && now-hs <= 180`. Any error ⇒ 0. Test hook `TEST_AWG_PEERS`.
- `getMeshAwgKernelStatus()` (telemetry.ts:356-376): `execAsync('modinfo amneziawg')` fails ⇒ `{status:'not_installed', version:''}`;
  version = `/^version:\s*(\S+)/m`; then `execAsync('lsmod | grep -w amneziawg')` ok ⇒ `'loaded'` else `'built_not_loaded'`.
  Never returns `'error'` (proto comment lists it). Cached 60 s (telemetry.ts:381-389), invalidated after a DKMS install.
  Orchestrator dependency: `meshAwgKernelStatus === 'loaded'` means installed (`orch:src/core/services/orchestrator/meshTunnelOrchestrator.ts:204`);
  `not_installed`/`built_not_loaded` trigger mesh warnings/self-repair (`orch:src/core/NodeWarningWorker.ts`, CLAUDE.md "Модуль ядра Mesh").
- `getMeshTunnelInterfaceStatus()` (telemetry.ts:438-461): `ip link show <meshIface>` fails ⇒ `{status:'absent', 0, []}`;
  then `awg show <meshIface> dump` ⇒ `{status:'up', ...parseMeshPeerDump}`; dump failure ⇒ `{status:'up', 0, []}`. Never `'down'`.
  Orchestrator: `liveMeshPeersOf` (`orch:src/shared/utils/meshRouting.ts:29-40`) — `'absent'|'down'` ⇒ empty set, `'up'` ⇒ keys,
  anything else ⇒ unknown (null); `up` + 0 keys + `activePeers>0` ⇒ unknown.
- `parseMeshPeerDump(stdout, nowSec)` (telemetry.ts:418-430): `stdout.trim().split('\n')`, skip line 0, each line `trim().split(/\s+/)`,
  ≥5 parts, live if `hs>0 && nowSec-hs <= 180` (inclusive). Returns keys in dump order. Tests: tests/meshPeerDump.test.ts:13-35
  (180 s old is live, 181 s is not, truncated line skipped).

---

## 5. RPC `GetAwgObservation`

Handler `getAwgObservationHandler`, src/services/awgObservation.service.ts:19-71; logic in src/utils/awgFlows.ts.
Orchestrator callers: `orch:src/zones/anomaly-traffic/services/connectionRecordsFeed.ts:208-235` (watcher feed, egress only,
fleet pass), `orch:src/core/services/awgPeerIntegrity.ts:167-190, 275-280` (daily peer drift check + recheck after heal).
Deadline **10 s** (`OBSERVATIONAL_RPC_DEADLINE_MS`, orch:GrpcConfigPusher.ts:80, 432-449).

### 5.1 Inputs
`AwgObservationRequest{orchestrator_secret}` — only for auth. The interface is **not** configurable: the handler calls
`collectAwgObservation()` with its default `iface = 'awg0'` (awgFlows.ts:197) — not `getAwgInterfaceName()`.

### 5.2 Flow (`collectAwgObservation`, awgFlows.ts:197-222)
1. `execAsync('awg show awg0 dump')` (shell, no timeout, default 1 MiB maxBuffer). Error ⇒ debug log, return
   `{peers:[], flows:[], conntrackAvailable:false}`.
2. `parseAwgDump(stdout)` (awgFlows.ts:78-107):
   - `stdout.trim().split('\n')`, drop first line (interface line), each line trimmed, skip empty, `split(/\s+/)`, need ≥ 8 parts.
   - `publicKey = parts[0]`; `endpoint = parts[2]==='(none)' ? '' : parts[2]`;
     `tunnelIps = parts[3]==='(none)' ? [] : parts[3].split(',').map(ip => ip.split('/')[0])`;
     `latestHandshakeUnix = parseInt(parts[4])`, `rxBytes = parseInt(parts[5])`, `txBytes = parseInt(parts[6])`, non-finite ⇒ 0.
     (`parseInt` semantics: leading digits; e.g. "12abc" ⇒ 12.)
3. Map tunnelIp → publicKey (later peers overwrite earlier on duplicate IP). Empty map ⇒ return `{peers, flows:[], conntrackAvailable:false}`.
4. `readConntrackTable()` (awgFlows.ts:240-261): first `fs.readFile('/proc/net/nf_conntrack','utf-8')`; on error debug log and
   fall back to `execAsync('conntrack -L', {maxBuffer: 32*1024*1024})` (stdout only; stderr summary ignored; no timeout).
   Both fail ⇒ `null` ⇒ `{peers, flows:[], conntrackAvailable:false}`.
5. `parseConntrack(contents, map)` (awgFlows.ts:139-166): for each non-empty line (not trimmed for regexes):
   first `/\bsrc=(\S+)/` → look up in map (skip if absent); first `/\bdst=(\S+)/` and first `/\bdport=(\d+)/` (skip if either missing);
   `protocol = protocolOf(line.trim().split(/\s+/))` — first of the first 4 tokens that is in
   `{tcp, udp, icmp, icmpv6, udplite, dccp, sctp, gre}`, else `'unknown'` (awgFlows.ts:123-137; handles both `/proc` format
   `ipv4 2 tcp 6 …` and `conntrack -L` format `tcp 6 …`). Push `{publicKey, tunnelIp: src, protocol, destinationIp: dst, destinationPort: parseInt(dport)}`.
   Only the FIRST src/dst pair (original direction) is used — the second is post-NAT and points at the node itself.
6. Return `{peers, flows, conntrackAvailable:true}`.

### 5.3 Response (awgObservation.service.ts:33-58)
- `success:true`; `message`: `'AWG interface is not active on this node.'` when `peers.length === 0` (also when the interface exists
  with zero peers), else `` `${peers} peer(s), ${flows} flow(s).` ``.
- `peers[]`: `{publicKey, endpoint, tunnelIps, rxBytes, txBytes, latestHandshakeUnix}` (proto `AwgPeerCounters`, field 6 = endpoint).
- `flows[]`: `{publicKey, tunnelIp, protocol, destinationIp, destinationPort}`.
- `conntrackAvailable`.
- Unauthorized: `{success:false, message:'Invalid orchestrator secret token.', peers:[], flows:[], conntrackAvailable:false}`.
- Exception: `{success:false, message:<err.message|'Unknown error'>, peers:[], flows:[], conntrackAvailable:false}` (practically unreachable).
- Response field names MUST be camelCase in the TS object (keepCase:false drops snake_case silently,
  awgObservation.service.ts:14-18; guarded by tests/responseFieldNaming.test.ts). In Go this is moot (generated structs).

Orchestrator semantics depending on this: `success:true` + `peers:[]` is interpreted as "the node has no peers" by the integrity
check → all DB peers become `missingKeys` → heal push (`orch:awgPeerIntegrity.ts:182-190`). A failed RPC (`success:false`)
puts the node in `unreachableNodes` instead. Keep the "interface absent ⇒ success:true, empty" behaviour.

Tests: tests/awgFlows.test.ts (whole file): interface line skipped; rx/tx columns; `(none)` endpoint ⇒ `''`; multiple allowed-ips
with masks stripped; `(none)` allowed ips ⇒ `[]`; <8 columns ⇒ dropped; junk input ⇒ `[]`; only owned flows; first dst used;
protocol by token search for both formats; incomplete lines skipped.

---

## 6. RPC `UploadMeshAwgKernelSource` + DKMS build (prerequisite of the mesh)

Handler `uploadMeshAwgKernelSourceHandler`, src/services/meshTunnel.service.ts:270-363 (client-streaming).
Orchestrator: `orch:src/core/services/grpc/GrpcBinaryUploader.ts:327-347` (64 KiB chunks, `targetBinary='amneziawg-kernel-src'`
(ignored by agent), **no deadline**), called from `meshTunnelOrchestrator.ts:375-388` (egress, only if telemetry kernel status ≠ `loaded`)
and `:439-449` (front, always). Failure is wrapped as `Не удалось установить kernel-модуль: <message>` and later digested by
`failureDigest`.

### 6.1 Streaming / auth
- `secretVerified = verifySecret(metadata secret)`. Per `data` event: if not verified, try `data.orchestratorSecret`.
  Still not verified ⇒ `isAborted=true`, warn, `callback(null,{success:false, message:'Invalid orchestrator secret token.'})`
  (in try/catch), then `call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'))`. Client observes OK +
  success:false (tests/app.test.ts:1140-1151).
- Temp names: `uniqueId = Date.now() + '_' + randomUUID().slice(0,8)`; `tarPath = <os.tmpdir()>/amneziawg_kernel_src_<id>.tar.gz`;
  `buildDir = <os.tmpdir()>/amneziawg_kernel_build_<id>` (meshTunnel.service.ts:278-285, 330). Comment: tmpdir is used because the
  path is handed to external tools.
- Chunks with `chunk.length>0` appended to a lazily-created write stream (backpressure ignored; no size cap). `version`, `is_final`,
  `target_binary` are ignored; processing starts on stream **end**.
- On `end`: flush stream; if `!secretVerified || bytesWritten === 0` ⇒ unlink tar, `{success:false, message:'No valid source data received or unauthorized.'}`.
- `error` / `cancelled` events: end stream, unlink tar (buildDir not touched).

### 6.2 Build (meshTunnel.service.ts:330-350, `installDkmsKernelModule` 208-264)
1. `mkdir -p buildDir`; `execAsync('tar -xzf "<tarPath>" --strip-components=1 -C "<buildDir>"', {timeout:60000})`; unlink tar.
2. `findDkmsConf(buildDir)` (44-64): first existing of `<buildDir>/src/dkms.conf`, `<buildDir>/dkms.conf`, then
   `<buildDir>/<each subdir in readdir order>/dkms.conf`; none ⇒ throw `dkms.conf not found in extracted source tarball under <buildDir>`.
3. `parseDkmsConf` (66-80): `/PACKAGE_NAME\s*=\s*"([^"]+)"/`, `/PACKAGE_VERSION\s*=\s*"([^"]+)"/` (first match, quoted values only);
   missing ⇒ warn and fallback `amneziawg` / `1.0.0`. `srcDir = dirname(dkms.conf)`.
4. `isDkmsModuleInstalled(name, version)` (184-191): `execAsync('dkms status -m <name> -v <version>')` (no timeout) →
   `isDkmsInstalledForKernel(stdout, os.release())`; error ⇒ false.
   `isDkmsInstalledForKernel` (93-99): any line where `line.split(/:\s*/, 2)` gives head and state, state.trim() matches
   `/^installed\b/i`, and `head.split(',').map(trim)` contains the exact kernel release. Handles both dkms formats
   (`amneziawg/1.0.0, 6.8.0-139-generic, x86_64: installed` and `amneziawg, 1.0.0, 6.8.0-139-generic, x86_64: installed`);
   `built`/`added` ⇒ false; `6.8.0-139-generic-64k` ≠ `6.8.0-139-generic` (tests/dkmsKernelStatus.test.ts:10-38).
   Incident (meshTunnel.service.ts:85-91): checking only "installed" anywhere skipped the build after a kernel upgrade and
   `modprobe` failed "Module amneziawg not found in directory /lib/modules/<new kernel>" (2026-09-28, 6.8.0-139-generic).
5. If installed for the running kernel ⇒ skip build (info log). Else, when NODE_ENV≠test:
   a. `removeStaleDkmsTrees(name)` (131-141):
      `execAsync('dkms status -m <name>', {timeout:30000})` (error ⇒ stdout ''); for each `dkmsKnownVersions(stdout, name)`
      (106-119: `line.split(':')[0].trim().split(',').map(trim)`; `parts[0]` starting `<name>/` ⇒ version after slash; or
      `parts[0]===name && parts[1]` ⇒ parts[1]; dedup, insertion order) run
      `execAsync('dkms remove -m <name> -v <version> --all', {timeout:60000})` (errors warn, continue);
      then `execAsync('dpkg --configure -a', {timeout:300000})` (error warn).
      Why (121-130): a hoster kernel upgrade made `linux-headers-*` postinst autoinstall the old amneziawg tree, which failed and
      broke `apt-get install` itself ("dpkg: error processing package linux-headers-…"), leaving packages half-configured forever.
   b. `execAsync('apt-get update -y', {timeout:120000})` — throws on failure.
   c. `execAsync('apt-get install -y dkms build-essential linux-headers-$(uname -r)', {timeout:300000})` — shell expands `$(uname -r)`; throws.
   d. `patchCompatForRunningKernel(srcDir)` (170-182): read `<srcDir>/compat/compat.h` and
      `/lib/modules/<os.release()>/build/include/net/udp_tunnel.h` (either missing ⇒ no-op). `patchCompatForKernelHeaders` (153-168):
      for `setup_udp_tunnel_sock` with header regex `/setup_udp_tunnel_sock\s*\(\s*struct\s+net\s*\*\s*\w*\s*,\s*struct\s+sock\s*\*/`
      and `udp_tunnel_sock_release` with `/udp_tunnel_sock_release\s*\(\s*struct\s+sock\s*\*/`: if the header matches and compat has
      a line matching `^[ \t]*#define[ \t]+<fn>\(.*\n?` (multiline, FIRST occurrence only), delete that line. Write compat.h back
      only if something was dropped. Why (143-151): compat.h decides by `LINUX_VERSION_CODE < 7.1.5`, Ubuntu backported the
      `struct sock *` signature into 7.0.0-38 ⇒ "incompatible pointer type". Must happen before `make dkms-install` because that
      copies the source to `/usr/src`. Tests: tests/dkmsKernelStatus.test.ts:56-99.
   e. `execAsync('make -C "<srcDir>" dkms-install', {timeout:60000})`.
   f. `execAsync('dkms add -m <name> -v <version>', {timeout:30000})`; on error, `out = (stderr||stdout||message).toLowerCase()`;
      tolerated iff `out.includes('already') && out.includes('add')`, else rethrow.
   g. `execAsync('dkms build -m <name> -v <version>', {timeout:180000})` then `execAsync('dkms install -m <name> -v <version>', {timeout:60000})`.
      On either error: read `/var/lib/dkms/<name>/<version>/build/make.log`, keep last 2000 chars (JS UTF-16 units) and throw
      `new Error(logTail ? '<err.message>\n\n--- dkms build log (tail) ---\n<logTail>' : '<err.message>')`.
6. Regardless of the skip, when NODE_ENV≠test: `execAsync('modprobe amneziawg', {timeout:15000})` (hard-coded name, not `pkg.name`);
   `execAsync('lsmod | grep -w amneziawg', {timeout:5000})`; `mkdir -p /etc/modules-load.d`; write `/etc/modules-load.d/amneziawg.conf`
   = `"amneziawg\n"` (best effort, warn on failure). Why (253-255): DKMS AUTOINSTALL rebuilds on new kernels but does not modprobe at boot.
7. `invalidateMeshAwgKernelStatusCache()` (also in test mode).
8. Response success: `{success:true, message:'AmneziaWG kernel module (<name> v<version>) installed and loaded'}`
   (test asserts `amneziawg` and `9.9.9-test` from fixture tests/fixtures/amneziawg-kernel-src-fixture.tar.gz, tests/app.test.ts:1153-1182).
   Failure: `{success:false, message:'Failed to install kernel module: <msg>'}`. `finally`: unlink tar, `rm -rf buildDir`.

Under NODE_ENV=test steps 5a-5g and 6 are skipped; tar extraction and dkms.conf parsing still run.

---

## 7. RPC `ConfigureMeshTunnel` — S2S AWG3 mesh (`awgmesh0`)

Handler `configureMeshTunnelHandler`, src/services/meshTunnel.service.ts:379-568.
Orchestrator: `orch:src/core/services/orchestrator/meshTunnelOrchestrator.ts:263-285` (`pushMeshConfig`), via
`orch:GrpcConfigPusher.ts:660-682` (deadline 60 s). Callers gate on a 3 s liveness probe (`MESH_LIVENESS_PROBE_MS`).

### 7.1 Inputs (proto `MeshTunnelConfigPayload`, proto/agent.proto:439-491)
Read: `privateKey, addressV4, addressV6, listenPort, mtu, jc, jmin, jmax, peers[{publicKey, allowedIps, endpoint, persistentKeepalive}],
s1..s4, h1..h4, headerProtectionKey, i1..i5`. `orchestrator_secret` is ignored (metadata auth).
What the orchestrator sends: `addressV4 '<tunnelIpV4>/16'`, `addressV6 '<tunnelIpV6>/64'`, `listenPort 51821`
(`orch:src/shared/constants/mesh.ts:6`), `mtu 1380`, `jc 4, jmin 40, jmax 70`, mimicry from the global template or omitted.
Peers (`buildMeshPeer`, meshTunnelOrchestrator.ts:117-133): on an egress node, front peers carry `endpoint '<addr>:51821'` and
keepalive 25; on a front, egress peers carry `endpoint ''` and keepalive 0 — **the egress always initiates** (TSPU reasoning,
meshTunnelOrchestrator.ts:96-115). The agent must therefore omit `Endpoint`/`PersistentKeepalive` lines for empty/zero values.

### 7.2 Config text (meshTunnel.service.ts:395-468)
- `meshConfigPath = config.MESH_AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/awgmesh0.conf'`; `iface = getMeshAwgInterfaceName()`
  (awg.ts:22-26: basename without `.conf`, fallback `awgmesh0`); `unitName = 'route-awg@' + iface`.
- Same `sanitizeConfigInput` as §1.2 (local copy, meshTunnel.service.ts:17-20).
- `cleanMtu = Number.isFinite(Number(mtu)) && Number(mtu) > 0 ? Math.trunc(Number(mtu)) : null` (meshTunnel.service.ts:425).
  Tests (tests/meshTunnelMtu.test.ts:62-91): 1380 ⇒ `MTU = 1380`; absent/0/-1 ⇒ no line; 1380.7 ⇒ `MTU = 1380`;
  `'1380\nPostUp = curl evil'` ⇒ NaN ⇒ no line.
- Layout:
  ```
  [Interface]
  PrivateKey = …          (non-empty)
  ListenPort = …          (listenPort truthy)
  Address = v4[, v6]      (each if non-empty — NO ipv6Mode filter here)
  MTU = …                 (cleanMtu)
  Jc = … / Jmin = … / Jmax = …   (always, see 0.1)
  S1..S4, H1..H4          (raw non-empty, sanitized value)
  I1..I5                  (sanitized non-empty)
  HeaderProtectionKey = … (sanitized non-empty)
  ```
  No RandomTrailers/DisableCookies here. Peers (skip null / empty sanitized publicKey):
  ```

  [Peer]
  PublicKey = …
  AllowedIPs = …              (allowedIps || allowed_ips, sanitized, if non-empty)
  Endpoint = …                (sanitized, if non-empty)
  PersistentKeepalive = …     (persistentKeepalive || persistent_keepalive, if truthy; raw value, not sanitized)
  ```
  `peerCount` counts written peers.
- Write: `mkdir -p dirname`, `writeFile` in place, default mode (same caveats as §1.4 step 4).

### 7.3 Apply (NODE_ENV≠test only; meshTunnel.service.ts:473-557)
1. `ensureMeshUdpPortAllowed(Number(listenPort))` (src/utils/firewall.ts:44-56): return if not integer 1..65535 or ufw missing;
   `execAsync('sudo ufw allow <port>/udp')` (info log with stdout; error warn). No reload. Not added to the active-ports cache, so
   `syncEgressFirewall` never closes it. Why (firewall.ts:32-43, 2026-09-28): only egress initiates; on a front with ufw the
   handshake to the closed port would be dropped.
2. Identity probe: `execAsync('ip -4 addr show dev <iface>')` ok ⇒ `interfaceExists`, `currentAddressV4` = first `/inet\s+(\d+\.\d+\.\d+\.\d+)\//`
   or null. Error ⇒ not exists.
3. `newAddressV4 = cleanAddressV4.split('/')[0] || null`.
4. If exists: `currentMtu` = `Number(readFile('/sys/class/net/<iface>/mtu').trim())` if finite, else null.
   `mtuUnchanged = cleanMtu === null || currentMtu === cleanMtu`.
5. `identityUnchanged = interfaceExists && currentAddressV4 !== null && currentAddressV4 === newAddressV4 && mtuUnchanged`.
   Why (476-487, 501-506): `awg-quick strip` drops `Address`/`MTU` (wg-quick-only directives) before `awg syncconf`, so a reload would
   leave disk and interface silently diverged. IPv6 address changes are NOT detected (only v4 compared).
6. identityUnchanged ⇒ `execAsync('systemctl reload <unit>')`; on error warn and `execAsync('systemctl restart <unit>')`; if that fails
   return `{success:false, message:'Failed to reload or restart mesh tunnel service (<unit>): <restartMsg>'}`
   (`restartMsg = (stderr||stdout||message||'systemctl restart error').trim()`).
   Else ⇒ `execAsync('systemctl restart <unit>')` (works also when not yet up — oneshot/RemainAfterExit); failure ⇒
   `{success:false, message:'Failed to apply new tunnel identity to mesh service (<unit>): <restartMsg>'}`.
7. `execAsync('systemctl enable <unit>')`; error warn.
8. Return `{success:true, message:'Mesh AWG tunnel interface <iface> configured with <peerCount> peer(s)'}`
   (test asserts substring `2 peer`, tests/app.test.ts:1196-1244). Outer catch: `{success:false, message:'Mesh tunnel configuration error: <msg>'}`.
   Unauthorized: `{success:false, message:'Invalid orchestrator secret token.'}`.

No iptables, no sysctl forwarding for the mesh. The mesh interface relies on the kernel module being loaded (awg-quick
uses netlink); no explicit check that the module is loaded before `systemctl restart`.

Test-mode file assertions (tests/app.test.ts:1196-1244): PrivateKey, ListenPort, Address joined, Jc, S1 `113`, S4 `0`
(string `'0'` is non-empty ⇒ written), H1/H4, HPK, I1/I2, **empty I3 not written**, both peers with Endpoint/keepalive.

---

## 8. RPC `ManageFirewall`

Handler `manageFirewallHandler`, src/services/firewall.service.ts:11-68. **No orchestrator call site exists**
(grep for `manageFirewall` in `orch:src` and `orch:tests` returns nothing; the RPC is reachable but unused). Keep it for contract parity.

- Inputs: `openUdpPorts`, `openTcpPorts` (repeated uint32). Each `.map(Number).filter(p => Number.isInteger(p) && p>0 && p<=65535)`;
  non-array ⇒ `[]`.
- NODE_ENV=test ⇒ `{success:true, message:'[TEST] Processed firewall ports: UDP=[<a, b>], TCP=[<c, d>]'}` (`join(', ')`).
- Else, sequentially: for each UDP `execAsync('sudo ufw allow <p>/udp')`, then each TCP `execAsync('sudo ufw allow <p>/tcp')`
  (errors logged, continue); then `execAsync('sudo ufw reload')` (warn on error). **No ufw-installed check**, no persistence/cache,
  never closes ports.
- Always `{success:true, message:'Successfully updated firewall rules: opened <nUdp> UDP and <nTcp> TCP ports.'}` — counts of
  valid requested ports, not of successes. Catch-all: `{success:false, message:'ManageFirewall error: <msg>'}`.
- Tests: tests/app.test.ts:1246-1266.

### 8.1 `syncEgressFirewall` (not an RPC; called from ApplyConfig, src/services/config.service.ts:180, after `sing-box check` passes)
src/utils/firewall.ts:140-196.
1. ufw missing (`command -v ufw` fails) ⇒ warn `ufw is not installed on this system; skipping egress firewall synchronization`, return.
2. `newPorts = extractUdpTunnelPorts(config)` (90-107): for each inbound object in `config.inbounds` (array, else none):
   skip if `typeof type === 'string'` and type ∉ {`hysteria2`,`tuic`} (an inbound **without** a string type is NOT skipped);
   skip if `typeof listen === 'string'` and `listen.trim()` ∈ {`127.0.0.1`,`::1`,`localhost`}; port = `Number(listen_port ?? port)`,
   integer 1..65535; dedupe, insertion order. Tests: tests/firewallSyncPlan.test.ts:77-97.
3. `previousPorts = readActivePortsCache()` (60-69): `/opt/route-agent/active_ports.json` parsed; non-array/unreadable ⇒ `[]`;
   non-number elements dropped.
4. `planFirewallSync(new, prev)` (127-135): `toOpen = new − prev` (order of `new`), `toClose = prev − new` (order of `prev`),
   `needsReload = toOpen.length>0 || toClose.length>0`. Order-insensitive. Tests: tests/firewallSyncPlan.test.ts:17-75.
   Why (115-126): before 2026-09-21 `ufw reload` ran on every config apply, i.e. on every subscriber creation, fleet-wide.
5. `!needsReload` ⇒ debug, return (cache NOT rewritten).
6. **Concurrently** (`Promise.allSettled`): `execAsync('sudo ufw allow <p>/udp')` for each toOpen; then concurrently
   `execAsync('sudo ufw delete allow <p>/udp')` for each toClose. Errors logged per port.
7. `execAsync('sudo ufw reload')` (stdout info, stderr warn, error logged).
8. `writeActivePortsCache(newPorts)` (73-76): mkdir -p `/opt/route-agent`, write `JSON.stringify(ports, null, 2)` (no trailing newline),
   in place. Written even if some ufw commands failed.
9. Any unexpected error ⇒ logged; never throws to ApplyConfig.

install.sh context (install.sh:104-134): installs `iptables iproute2 ufw … conntrack`, allows 22/tcp, 443/tcp, 443/udp, `$PORT`/tcp,
`ufw --force enable`, and writes `/etc/sudoers.d/route-agent-ufw` = `ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw` (0440). The agent
itself runs as root (install.sh:286), so `sudo` is redundant but present in every ufw command string.

---

## 9. RPC `ApplyTrafficThrottle` — tc ingress policer per source prefix

Handler `applyTrafficThrottleHandler`, src/services/trafficThrottle.service.ts:16-70; mechanism src/utils/trafficShaper.ts.
Orchestrator: `orch:src/zones/anomaly-traffic/services/throttleDelivery.ts:85-145` — every node (egress + fronts) through
`fleetPass`, full list every time (normally **empty** ⇒ clear path), deadline 10 s. Rates `'5mbit'` (soft) / `'1mbit'` (hard),
burst `'500k'`. Uses `success`, `message` (logged), `counters[]` (aggregated by prefix, int64 strings via `grpcInt64`).
`changed`/`applied` are not used by the orchestrator.

### 9.1 Input normalisation (trafficThrottle.service.ts:28-35)
`entries` (array else `[]`) → `{prefix, rate, burst}` each = `typeof === 'string' ? trim() : ''`; drop entries with empty prefix.
No CIDR/rate syntax validation (tc rejects bad ones per filter).

### 9.2 Order of operations (handler)
1. Auth fail ⇒ `{success:false, message:'Invalid orchestrator secret token.', changed:false, applied:0, counters:[]}`.
2. `counters = readThrottleCounters()` **first** (rebuild resets tc counters; trafficThrottle.service.ts:9-14):
   `readState()`; no state / no entries ⇒ `[]`; else `execFileAsync('tc', ['-s','filter','show','dev',state.interface,'parent','ffff:'])`
   ⇒ `parseFilterCounters(stdout, state.entries.map(e=>e.prefix))`; tc error ⇒ warn, `[]` (trafficShaper.ts:155-165).
3. `entries.length === 0` ⇒ `clearThrottle()` and return `{success:true, message:'Cleared', changed:true, applied:0, counters}`
   (`changed` is always true here, even if nothing was installed).
4. Any entry with empty rate or burst ⇒ `{success:false, message:'Entries without rate/burst: <prefixes joined ", ">', changed:false, applied:0, counters}`
   (nothing changed; why: "придушить кого-то наугад хуже, чем отказаться", 39-40).
5. `result = applyThrottle(entries)` ⇒ `{success:true, message: result.message, changed, applied, counters}`.
6. Any throw (e.g. `tc qdisc replace` failing, state write failing, `fs.rm` failing) ⇒ warn,
   `{success:false, message:<err.message>, changed:false, applied:0, counters:[]}` — pre-read counters are discarded.

### 9.3 `resolveClientFacingInterface()` (trafficShaper.ts:69-78)
`execFileAsync('ip', ['-o','route','show','default'])`; first `/\bdev\s+(\S+)/`; error ⇒ error log, null. Re-resolved on every
apply (never cached; why: provider/network rebuild can rename it).

### 9.4 `applyThrottle(entries)` (trafficShaper.ts:178-218)
1. iface null ⇒ return `{changed:false, applied:0, message:'No default-route interface found'}` — the handler reports this as
   **success:true**.
2. `desired = {interface: iface, entries: normalizeEntries(entries)}` — sort by `prefix.localeCompare` (ICU collation, NOT byte order;
   trafficShaper.ts:148-152). Duplicates kept.
3. `isSameThrottle(readState(), desired)` (136-145: same interface, same length, same prefix/rate/burst at every index) ⇒
   return `{changed:false, applied: desired.entries.length, message:'Unchanged'}` — no tc calls (preserves drop counters, the
   escalation evidence).
4. `execFileAsync('tc', ['qdisc','replace','dev',iface,'handle','ffff:','ingress'])` — throws to handler on failure.
5. `execFileAsync('tc', ['filter','del','dev',iface,'parent','ffff:'])` — errors ignored (no filters).
6. For each entry `index` (0-based): `isV6 = prefix.includes(':')`;
   `execFileAsync('tc', ['filter','add','dev',iface,'parent','ffff:','protocol', isV6?'ipv6':'ip','prio', String(index+1),
   'u32','match', isV6?'ip6':'ip','src',prefix,'police','rate',rate,'burst',burst,'conform-exceed','drop/ok'])`;
   success ⇒ `applied++`; failure ⇒ warn, continue (one bad prefix must not cancel the layout).
7. `writeState(desired)` (130-133): mkdir -p `/etc/route-agent`, write `JSON.stringify(state, null, 2) + "\n"` in place.
   Written even when some filters failed.
8. Return `{changed:true, applied, message:'Applied'}`.
   There is a sub-second window between del and add where no limit applies (documented as the safe direction, 174-176).

### 9.5 `parseFilterCounters(output, prefixes)` (trafficShaper.ts:91-120)
Line scan: `/\bpref\s+(\d+)\s+u32\b/` sets `currentPref` (each filter prints 3 such lines); `/\bSent\s+(\d+)\s+bytes\s+(\d+)\s+pkts?\s+\(dropped\s+(\d+)/`
with a current pref ⇒ `prefix = prefixes[pref-1]`; if defined push `{prefix, sentBytes, sentPackets, droppedPackets}`; reset
`currentPref = null` in both cases. Sent line with no current pref ⇒ ignored. Unknown pref ⇒ skipped (never attributed to another prefix).
Tests: tests/trafficShaper.test.ts:17-90 (verbatim kernel 6.6.87.2 output fixture), 92-133 (isSameThrottle incl. order sensitivity).

### 9.6 `clearThrottle()` (trafficShaper.ts:221-229)
`iface = state?.interface ?? resolveClientFacingInterface()`; null ⇒ return. `execFileAsync('tc', ['qdisc','del','dev',iface,'ingress'])`
(errors ignored); `fs.rm(STATE_FILE, {force:true})`. Runs on every empty delivery (the normal steady state).

Proto rationale (proto/agent.proto:165-183): ingress policer, not egress shaper, because sing-box terminates and re-originates
connections; drops are the signal (cooperative TCP 122 drops vs UDP flood 1,791,515); burst is load-bearing (8mbit gave 4–11 Mbit/s
TCP goodput depending on burst).

---

## 10. Kernel tuning (startup, not an RPC) — src/utils/kernelTuning.ts

Called as `void applyKernelTuning()` after the gRPC server binds, NODE_ENV≠test only (src/index.ts:201-216). Not awaited; result only logged.

### 10.1 Profile (kernelTuning.ts:79-110), applied in this order
| key | value |
|---|---|
| net.core.rmem_default | 1048576 |
| net.core.wmem_default | 1048576 |
| net.core.rmem_max | 7500000 |
| net.core.wmem_max | 7500000 |
| net.ipv4.tcp_congestion_control | bbr |
| net.core.default_qdisc | fq |

Deliberately absent (tests/kernelTuning.test.ts:68-79, 230-245): `net.ipv4.udp_mem`, `tcp_rmem`, `tcp_wmem` (RAM-derived),
`net.core.netdev_max_backlog` (softnet drops were zero), `net.ipv4.ip_local_port_range` (would swallow fixed ports
20000/28080/28081/29000/29001). `rmem_max` is not pre-doubled (kernel doubles internally; kernelTuning.ts:50-53). `rmem_default` is the
real lever: dropping sockets never call setsockopt (kernelTuning.ts:23-40). `default_qdisc` only affects newly created qdiscs
(i.e. after reboot), deliberately not forced on live interfaces (66-70).

### 10.2 Steps (`applyKernelTuning(runExecFile = execFileAsync, entries)`, 146-180)
1. `writeSysctlFile` (183-194): target `config.SYSCTL_PROFILE_PATH` (default `/etc/sysctl.d/99-route-agent.conf`, src/config.ts:115);
   expected = `renderSysctlFile()`; if existing content byte-equal ⇒ `fileChanged=false`; else mkdir -p, writeFile (in place) ⇒ true.
   Write errors propagate (the promise rejects; since it is `void`ed, this becomes an unhandled rejection — see R14).
2. For each entry: `execFile('sysctl', ['-w', '<key>=<value>'])`; on error push `{key, applied:false, effective:null, error:<message>}`, continue.
   Else `execFile('sysctl', ['-n', key])` → `effective = stdout.trim().replace(/\s+/g,' ')`; read failure ⇒ `effective:null` but `applied:true`.
3. Log `{fileChanged, applied, failed}`.

`renderSysctlFile` (130-133) = `"# Управляется route-agent, правки будут перезаписаны при следующем старте агента.\n\n"` +
entries mapped to `"# <why>\n<key> = <value>"` joined by `"\n\n"` + `"\n"`. Current output: 1356 bytes,
SHA-256 `0a308f06ba9ed1e728fed4a578caee3371ea2ef184b8a72bd18040a33be3cc08`. Exact text:

```
# Управляется route-agent, правки будут перезаписаны при следующем старте агента.

# ГЛАВНАЯ строка: буфер сокетов, которые его не запрашивают. Именно они теряют пакеты (по 500-780 тысяч на сокет), сидя на умолчании 212992 при уже поднятом потолке
net.core.rmem_default = 1048576

# то же для отправки: SndbufErrors на нагруженной ноде 316 тысяч
net.core.wmem_default = 1048576

# потолок для тех, кто буфер запрашивает явно. Значение из вики quic-go; сам по себе теряющие сокеты не спасает — они до потолка не тянутся
net.core.rmem_max = 7500000

# то же для отправки
net.core.wmem_max = 7500000

# на нетюненой ноде cubic; bbr доступен в ядре (tcp_available_congestion_control: reno cubic bbr)
net.ipv4.tcp_congestion_control = bbr

# пара к bbr: пейсинг делает очередь, а не ядро. На нетюненой ноде fq_codel. Действует на вновь создаваемые qdisc, то есть с перезагрузки
net.core.default_qdisc = fq
```
Byte-equality matters only to avoid one rewrite on the first Go start; keep the golden SHA in a test.

Tests: tests/kernelTuning.test.ts (all): every key written with its reason; `-w` then `-n` per key; effective from kernel
(`7499776` stays `7499776`); read-only key does not stop others; failed read-back ≠ failed apply; file rewritten only when different.

Note the ConfigureAwg-time `sysctl -w net.ipv4.ip_forward=1` / `net.ipv6.conf.all.forwarding=1` (§1.4) is separate and not in this profile.

---

## 11. Timeouts summary

| Command | Timeout | Source |
|---|---|---|
| everything in ConfigureAwg, ConfigureMeshTunnel, ManageFirewall, syncEgressFirewall, health check, observation | none | §1, §7, §8, §4, §5 |
| `conntrack -L` | none, maxBuffer 32 MiB | awgFlows.ts:194, 252 |
| `awg show awg0 dump` | none, maxBuffer 1 MiB | awgFlows.ts:200 |
| tc / ip (shaper), sysctl (tuning) | none (execFile) | trafficShaper.ts, kernelTuning.ts |
| tar extract | 60 s | meshTunnel.service.ts:333 |
| `dkms status -m N` | 30 s | :132 |
| `dkms remove … --all` | 60 s | :134 |
| `dpkg --configure -a` | 300 s | :138 |
| `apt-get update -y` | 120 s | :219 |
| `apt-get install -y dkms build-essential linux-headers-$(uname -r)` | 300 s | :220 |
| `make -C … dkms-install` | 60 s | :224 |
| `dkms add` | 30 s | :227 |
| `dkms build` | 180 s | :240 |
| `dkms install` | 60 s | :241 |
| `modprobe amneziawg` | 15 s | :250 |
| `lsmod | grep -w amneziawg` | 5 s | :251 |
| `dkms status -m N -v V` (installed check) | none | :186 |

Orchestrator-side deadlines: ConfigureAwg/ConfigureMeshTunnel 60 s; GetAwgObservation/ApplyTrafficThrottle 10 s; uploads none.

---

## 12. Parity checklist (testable)

AWG config (ConfigureAwg)
1. Unauthorized (bad metadata; payload has no secret) ⇒ OK status, `success=false`, message exactly `Invalid orchestrator secret token.`.
2. Omitted jc/jmin/jmax ⇒ file contains `Jc = 0`, `Jmin = 0`, `Jmax = 0`.
3. Directive order exactly §1.4 step 3; peers block preceded by a blank line; no Endpoint/keepalive for client peers.
4. `ipv6Mode='ipv4-only'` drops the v6 address from `Address`; any other value (incl. `''`) keeps it.
5. `RandomTrailers`/`DisableCookies`: `on|1|ON` ⇒ `on`, `off|0|OFF` ⇒ `off`, other/empty/injection ⇒ no line.
6. CR/LF stripped and values trimmed in every string field; S/H with raw `"0"` written as `S1 = 0`; I-fields empty ⇒ omitted.
7. `port=0` ⇒ no `ListenPort` line, success message still says `port 51820`; message always says `awg0`.
8. Disable ⇒ `systemctl stop … || … || true` chain, ufw delete+reload if port and ufw, message `AmneziaWG (AWG3) service successfully disabled and stopped.`; file kept.
9. Existing iface ⇒ reload, fallback restart; missing ⇒ start; failure messages exactly as §1.4 step 6; then enable; iptables FORWARD/MASQUERADE with `-C || -A`; ip6tables REJECT only for `trap-ipv6`; `ufw allow <port>/udp` + `ufw reload` when port and ufw.
10. Write/other exception ⇒ `AmneziaWG configuration error: <msg>`.

Unit file
11. Byte-exact unit content (§2); rewrite + daemon-reload only on difference; return true/false.

Health check
12. Status strings `inactive` / `nominal` / `nominal (restarted Nm ago|Nh ago)` (<24 h) / `crashed` / `crashed (restarted …)`.
13. Restart at most once per 5 min **after a successful restart**; failed restart not rate-limited (reproduce or consciously change — see R6).
14. Check runs on 30 s timer and on every telemetry tick; mesh iface never auto-restarted.

Observation
15. Dump parsing per §5.2 (≥8 cols, `(none)` handling, mask stripping, NaN⇒0); conntrack first-pair, protocol token search in first 4 tokens, `unknown` fallback.
16. `/proc/net/nf_conntrack` first, then `conntrack -L` (32 MiB cap); neither ⇒ `conntrackAvailable=false` with peers still returned.
17. No peers mapped (zero tunnel IPs) ⇒ conntrack not read, `conntrackAvailable=false`.
18. Interface missing ⇒ `success=true`, message `AWG interface is not active on this node.`, empty lists.
19. Message `"<n> peer(s), <m> flow(s)."` otherwise. Always uses iface `awg0`.

Kernel module upload / DKMS
20. Secret accepted from metadata or from any chunk until verified; unauthorized ⇒ success=false message `Invalid orchestrator secret token.`.
21. Zero bytes ⇒ `No valid source data received or unauthorized.`.
22. dkms.conf search order and regex parsing with fallbacks `amneziawg`/`1.0.0`.
23. `isDkmsInstalledForKernel`, `dkmsKnownVersions`, `patchCompatForKernelHeaders` exactly as tests/dkmsKernelStatus.test.ts.
24. Skip build iff installed for `uname -r`; otherwise exact command sequence and timeouts of §6.2/§11; `dkms add` "already…add" tolerance; build-log tail (last 2000 chars) appended with the exact separator `\n\n--- dkms build log (tail) ---\n`.
25. modprobe + lsmod + `/etc/modules-load.d/amneziawg.conf` (`amneziawg\n`) even when the build was skipped; kernel-status cache invalidated.
26. Messages `AmneziaWG kernel module (<name> v<version>) installed and loaded` / `Failed to install kernel module: <Command failed: …\n<stderr>…>`; temp tar and build dir always removed.

Mesh
27. MTU: positive finite ⇒ truncated integer line; 0/negative/non-numeric ⇒ no line.
28. Peers: Endpoint/PersistentKeepalive omitted when empty/0; AllowedIPs verbatim.
29. `ensureMeshUdpPortAllowed(listenPort)` before systemctl (no reload; not cached).
30. Reload only if iface exists AND current IPv4 == new IPv4 AND (no MTU requested OR MTU equal); otherwise restart; failure messages per §7.3; then enable.
31. Success message `Mesh AWG tunnel interface <iface> configured with <n> peer(s)`.

Firewall
32. ManageFirewall: port filtering, sequential allows, reload, always-success message with requested counts; test-mode message format.
33. syncEgressFirewall: `extractUdpTunnelPorts` incl. "no type ⇒ counted" and loopback skip; plan math; no-op without changes (no reload, no cache write); concurrent allow/delete, reload, cache rewrite (pretty JSON, no trailing NL).

Shaper
34. Counters read before any change; returned on Cleared/incomplete/Applied/Unchanged; `[]` on exception.
35. Empty list ⇒ clear (`tc qdisc del dev <iface> ingress`, rm state) and `changed=true`, message `Cleared`.
36. Missing rate/burst ⇒ `Entries without rate/burst: p1, p2`, success=false, nothing touched.
37. Identical normalized state (iface + ordered entries) ⇒ `Unchanged`, `applied=len`, no tc calls.
38. Rebuild: `qdisc replace … ingress`, `filter del` (ignore error), per-entry `filter add` with prio=index+1 and `ip`/`ip6` by `:`; state JSON + `\n`.
39. No default route ⇒ success=true, message `No default-route interface found`, changed=false, applied=0.
40. `parseFilterCounters` per tests/trafficShaper.test.ts (3 lines per pref, unknown pref skipped, stray Sent ignored).
41. Sort order identical to JS `localeCompare` for the prefix alphabet (see R11).

Kernel tuning
42. Golden sysctl file (SHA above), per-key `sysctl -w` then `-n`, non-fatal failures, no rewrite when identical, runs once after bind, not awaited.

---

## 13. Risks / smells (flag only — not fixed)

- **R1 (high) `ExecReload` can never succeed on Debian/Ubuntu.** `ExecReload=/bin/sh -c '… syncconf %i <(… strip %i)'`
  (systemdUnit.service.ts:29) uses bash process substitution; `/bin/sh` is dash there (`dash -c 'cat <(echo hi)'` ⇒
  `Syntax error: "(" unexpected`, verified in this sandbox). So every `systemctl reload route-awg@*` fails and both ConfigureAwg
  and ConfigureMeshTunnel fall back to `systemctl restart` — dropping all live sessions on every push. This *accidentally* makes
  ConfigureAwg apply `Address` changes (it has no identity check; the orchestrator's `awg_subnet6` op depends on it) and makes the
  mesh "live resync" a full restart. A Go port that "fixes" the shell (bash) would silently stop applying AWG Address/MTU changes.
  Decide consciously; at minimum add ConfigureAwg identity detection before switching to bash. Upstream `wg-quick@.service` uses `/bin/bash`.
- **R2 (high) Private keys world-readable.** AWG and mesh configs are written with default mode (0644 under umask 022) and in place
  (non-atomic) — config.service.ts:640, meshTunnel.service.ts:471. awg-quick warns about world-accessible configs. Torn writes possible.
- **R3 (high) Disable is undone by the self-healer.** ConfigureAwg(enabled=false) stops the unit but leaves the config file and the
  enabled unit; `checkAndHealAwgInterface` treats "config file exists" as configured and restarts `route-awg@awg0` within ~2–30 s
  (telemetry.ts:496-543). Also re-enabled at boot (unit stays enabled).
- **R4 (medium) Shell interpolation.** `execAsync` strings embed values from files/network: `dkms add/build/install/remove -m <name> -v <version>`
  where name/version come from the uploaded `dkms.conf` (`[^"]+` allows spaces, `;`, `$()`), `make -C "<srcDir>"` (dir names from the
  tarball), `dkms status -m <name>`. ufw/iptables commands interpolate only numbers or the `ip route` iface name. The tarball is
  orchestrator-supplied (trusted channel, mTLS) but this is still a root shell sink. Prefer execve in Go. tc/ip/sysctl already use execFile.
- **R5 (medium) Hard-coded `awg0`** in iptables rules, `ip6tables`, the ConfigureAwg success message, `GetAwgObservation`
  (`collectAwgObservation()` default) and `getAwgActivePeersCount` while unit/health use `getAwgInterfaceName()` (config-derived).
  Changing `AWG_CONFIG_PATH` breaks NAT and observation silently.
- **R6 (medium) Health check hammering.** Runs on every telemetry tick (2 s per stream) plus 30 s timer, with overlapping async ticks and
  no lock (two concurrent checks can both restart). A *failed* restart records no cooldown ⇒ `systemctl restart` retried every ~2 s.
- **R7 (medium) No persistence of forwarding/NAT.** `ip_forward`, IPv6 forwarding, iptables FORWARD/MASQUERADE/ip6tables rules are
  runtime-only; after reboot the unit comes up (enabled) but AWG traffic is not forwarded/NATed until the next ConfigureAwg.
  MASQUERADE has no source match (masquerades all forwarded traffic on the default iface). `dual-stack` gets no IPv6 NAT with ULA addresses.
- **R8 (medium) No command timeouts** in ConfigureAwg/ConfigureMeshTunnel/ManageFirewall/health/observation; a hung `systemctl` or
  `ufw` outlives the orchestrator's 60 s/10 s deadline. Node `exec` timeouts kill only `/bin/sh`, not `apt-get`/`dkms` children.
- **R9 (medium) Mesh identity check ignores IPv6** (only v4 compared) and "MTU removed (0)" (keeps the old MTU until a restart).
- **R10 (medium) Shaper state written even when filters failed** (trafficShaper.ts:215) ⇒ the next identical delivery returns
  `Unchanged` and never retries the failed prefix. Interface change leaves the old iface's ingress qdisc/filters in place forever.
  `tc filter del … parent ffff:` removes foreign filters too. `No default-route interface found` is reported as `success:true`.
  On exception the already-read counters are discarded (`counters: []`).
- **R11 (low) Locale-dependent sort.** `normalizeEntries` uses `String.prototype.localeCompare` (ICU), which orders `:` before digits,
  unlike byte order. Go must replicate (e.g. `golang.org/x/text/collate` root) or accept one rebuild (and one counter reset) per changed
  ordering after the upgrade; prio↔prefix mapping is consistent either way because the state file stores the order.
- **R12 (low) ufw races.** `syncEgressFirewall` runs `ufw allow`/`ufw delete` concurrently (Promise.allSettled) — ufw has no robust
  locking on `user.rules`. Cache is written even if some commands failed (never retried). `ufw reload` after `ufw allow` is redundant
  (allow is live) but runs on every ConfigureAwg/ManageFirewall. ConfigureAwg disable `ufw delete allow <port>/udp` can remove a rule
  that install.sh or another feature also needs if ports collide.
- **R13 (low) Kernel-module side effects on client AWG (undetermined).** Once `amneziawg` is loaded for the mesh, upstream awg-quick
  prefers `ip link add type amneziawg` (kernel) over amneziawg-go, so `awg0` may switch to the kernel implementation on its next restart.
  Also `restartActiveAwgServices` (after awg tools/go uploads) restarts the mesh unit too.
- **R14 (low) `void applyKernelTuning()`** — a `writeFile` failure on the sysctl profile rejects an un-awaited promise (unhandled
  rejection; no `unhandledRejection` handler exists in src/, and Node ≥15 defaults to `--unhandled-rejections=throw`, so this
  would crash the agent, after which systemd restarts it and it fails again). Go: log and continue.
- **R15 (low) Upload handler**: no size cap, ignores stream backpressure, `callback` + `call.destroy` double-signal on auth failure;
  `modprobe amneziawg` hard-codes the module name instead of `PACKAGE_NAME`; `tar` extraction of an orchestrator-supplied archive into
  /tmp as root (GNU tar strips `..`/leading `/`, but symlink members are not filtered).
- **R16 (low) Misc.** `sudo` used although the agent is root; `sudoers` grants `ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw` to every user
  (install.sh:131-134). `AWG_RELOAD_COMMAND` config unused. Unit uses `${AWG_TOOLS_BINARY_PATH}-quick`, not `AWG_QUICK_BINARY_PATH`.
  awgFlows.ts:27-29 claims install.sh does not install `conntrack-tools`; install.sh:107 does install `conntrack`. `/proc/net/nf_conntrack`
  prints IPv6 addresses unabbreviated while `awg show dump` prints them abbreviated ⇒ IPv6 flows never match (undetermined on hosts);
  `conntrack -L` defaults to IPv4 only. Proto documents `mesh_tunnel_interface_status` `"down"` and kernel status `"error"`, which the
  agent never emits. ManageFirewall has no caller in the orchestrator.
- **R17 (low) No concurrency control** across ConfigureAwg/ConfigureMeshTunnel/ApplyTrafficThrottle: concurrent calls interleave file
  writes and systemctl/tc sequences (orchestrator dedupes AWG pushes per node in `rebuildInFlight`, but nothing on the agent).

---

## 14. Undetermined (needs a live node to confirm)

- Production distro `/bin/sh` (dash assumed: apt, `linux-headers-$(uname -r)`, Ubuntu kernels in incidents) — determines R1 in practice;
  check `journalctl -u route-awg@awg0` for reload failures.
- Whether `ufw reload` preserves the agent's manually appended FORWARD/MASQUERADE rules (believed yes: ufw manages its own chains), and
  whether return traffic to `awg0` relies on ufw's `ufw-before-forward` ESTABLISHED,RELATED accept under `DEFAULT_FORWARD_POLICY="DROP"`.
- Whether the delivered `awg-quick` script picks the kernel module for `awg0` once loaded (R13).
- grpc-js wire behaviour of `callback(...)` followed by `call.destroy(...)` in the upload auth failure (tests only show OK + success:false).
- IPv6 matching in conntrack (R16).
