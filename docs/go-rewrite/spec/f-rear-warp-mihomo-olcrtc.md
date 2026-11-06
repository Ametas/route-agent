# Spec F — Rear proxy (sing-box / mihomo), WARP guard, mihomo uploads, Olcrtc

Status: behavioral spec of the TypeScript agent on branch `go-rewrite`, written as input for a Go port.
Nothing here proposes changes; "Risks / smells" sections only flag.

Path conventions: `agent:` = `/home/user/route-agent`, `orch:` = `/home/user/vpn-orchestrate`.
All line numbers are against the current working trees.

## 0. Scope and shared plumbing

| RPC / loop | Handler | File |
|---|---|---|
| `ConfigureRearSingbox` (unary) | `configureRearSingboxHandler` | agent:src/services/rearSingbox.service.ts:180-307 |
| `GetRearConnections` (unary) | `getRearConnectionsHandler` | agent:src/services/singboxConnections.service.ts:58-79 (+ src/utils/rearConnections.ts) |
| `GetWarpKeyHealth` (unary, adjacent — shares rear Clash API discovery) | `getWarpKeyHealthHandler` | agent:src/services/warpKeyHealth.service.ts:26-84 (+ src/utils/warpKeyHealth.ts) |
| `PullNodeWarnings` — rear part only | `detectEmptyRearRuleProviders` | agent:src/utils/rearRuleProviderWarning.ts:73-113, called from src/services/nodeWarnings.service.ts:45-53 |
| `UploadMihomoBinary` (client stream) | `uploadMihomoBinaryHandler` | agent:src/services/mihomo.service.ts:58-120 |
| `UploadRearRuleSet` (client stream) | `uploadRearRuleSetHandler` | agent:src/services/mihomo.service.ts:137-203 |
| `SyncOlcrtcInstance` (unary) | `syncOlcrtcInstanceHandler` | agent:src/services/olcrtc.service.ts:152-218 |
| `DeleteOlcrtcInstance` (unary) | `deleteOlcrtcInstanceHandler` | agent:src/services/olcrtc.service.ts:225-260 |
| `StreamOlcrtcEvents` (server stream) | `streamOlcrtcEventsHandler` | agent:src/services/olcrtc.service.ts:271-356 |
| `UploadOlcrtcAgentSrvBinary` (client stream) | `uploadOlcrtcAgentSrvBinaryHandler` | agent:src/services/olcrtc.service.ts:363-490 |
| Rear WARP guard timer | `startRearWarpGuard` / `guardOnce` | agent:src/services/rearWarpGuard.service.ts:30-95, src/utils/rearWarpGuard.ts |
| Boot-time olcrtc unit provisioning | `ensureOlcrtcAgentSrvSystemdUnit()` | agent:src/index.ts:121 |

Registrations: agent:src/index.ts:153-176. The guard is started after bind, only when `NODE_ENV !== 'test'`: agent:src/index.ts:203-216.

### 0.1 Proto loading — wire field naming (load-bearing)

- Agent loads proto with `keepCase:false, longs:String, enums:String, defaults:true, oneofs:true` (agent:src/index.ts:51-57). Orchestrator uses the same (`keepCase:false`, `longs:String`, `defaults:true`; orch:src/core/services/grpc/GrpcConnectionManager.ts:18-21).
- Consequences the Go port must keep in mind:
  - Request fields always arrive camelCase and with proto3 defaults filled (`''`, `0`, `false`). Every `call.request.snake_case` fallback in handlers (e.g. `config_json` at rearSingbox.service.ts:190, `user_short_uuid` at olcrtc.service.ts:162, `room_id` at :53) is **dead code**; the camel branch is always used.
  - Response object keys MUST be camelCase in TS; a snake_case-only key is silently dropped by proto-loader (agent:tests/responseFieldNaming.test.ts:11-31 — "skipped_reason приезжает как `""`"). In Go with generated structs this class of bug disappears; just make sure the same proto fields are populated.
  - int64/uint64 fields arrive at the orchestrator as numeric strings (`longs:String`); orchestrator normalizes (`normalizeGrpcTimestamp`, `Number(event.bytesIn)`; orch:src/core/services/grpc/GrpcOlcrtcEventStreamManager.ts:135-166). Go just sends integers.

### 0.2 Auth

agent:src/middleware/auth.ts:
- `verifySecret(token)` (:7-18): false if token empty/non-string; expected = `config.EGRESS_CONTROL_SECRET || ''`; false if byte lengths differ **or expected is empty**; else `crypto.timingSafeEqual`.
- `extractSecretFromMetadata(call)` (:23-26): first value of metadata key `x-orchestrator-secret`, stringified, else `''`.
- `authenticateCall(call)` (:31-39): metadata secret first; else `call.request.orchestratorSecret || call.request.orchestrator_secret` (payload field 1 of every request message in scope).
- Two auth styles are used in scope:
  - `authenticateCall` (metadata OR payload): ConfigureRearSingbox, GetRearConnections, GetWarpKeyHealth, StreamOlcrtcEvents, PullNodeWarnings.
  - **metadata ONLY** (`extractSecretFromMetadata` + `verifySecret`): SyncOlcrtcInstance (olcrtc.service.ts:156-160), DeleteOlcrtcInstance (:229-233). A payload-only secret is rejected there even though the proto has `orchestrator_secret = 1`. The orchestrator always sends metadata (orch:src/core/services/grpc/GrpcConnectionManager.ts:329-333), and does NOT put the secret in the olcrtc payloads (orch:src/core/services/orchestrator/systemOrchestrator.ts:795-808), so payload-only would never work anyway.
  - Client streams (UploadMihomoBinary, UploadRearRuleSet, UploadOlcrtcAgentSrvBinary): metadata, or `orchestratorSecret` on the **first data message that arrives**; an unauthenticated first chunk aborts (see §5.1).

### 0.3 Command execution

agent:src/utils/exec.ts: `execAsync = promisify(child_process.exec)` (runs via `/bin/sh -c`), `execFileAsync = promisify(execFile)` (no shell). **No timeout is ever passed** in this scope; Node defaults apply: no timeout, `maxBuffer` 1 MiB (exceeding it kills the child and rejects). Rejection object has `.stdout`, `.stderr`, `.message` (`Command failed: <cmd>\n<stderr>`), `.code`.

`NODE_ENV === 'test'` short-circuits many side effects (listed per RPC). In Go these test seams can be replaced by injectable runners, but the production behavior is the non-test branch.

### 0.4 Config keys used (agent:src/config.ts)

| Key | Default | Line |
|---|---|---|
| `SINGBOX_CONFIG_PATH` | `/etc/sing-box/config.json` | :10 |
| `SINGBOX_BINARY_PATH` | `/usr/local/bin/sing-box` | :11 |
| `REAR_SINGBOX_CONFIG_PATH` | `/etc/route-agent/rear.json` | :35 |
| `REAR_SINGBOX_UNIT_FILE_PATH` | `/etc/systemd/system/route-rear-singbox.service` | :36 |
| `MIHOMO_BINARY_PATH` | `/usr/local/bin/mihomo` | :42 |
| `REAR_MIHOMO_CONFIG_PATH` | `/etc/route-agent/rear.yaml` | :43 |
| `REAR_MIHOMO_UNIT_FILE_PATH` | `/etc/systemd/system/route-rear-mihomo.service` | :44 |
| `REAR_RULE_SET_DIR` | `/etc/route-agent/rules` | :46 |
| `REAR_WARP_GUARD_INTERVAL_MS` | `30000` (`z.coerce.number`) | :50 |
| `OLCRTC_AGENT_SRV_BINARY_PATH` | `/usr/local/bin/olcrtc-agent-srv` | :78 |
| `OLCRTC_AGENT_SRV_UNIT_FILE_PATH` | `/etc/systemd/system/olcrtc-agent-srv@.service` | :79 |
| `OLCRTC_AGENT_SRV_CONFIG_DIR` | `/etc/olcrtc-agent-srv` | :80 |

Note `rearCore.ts` re-applies fallbacks (`config.X || '<default>'`, rearCore.ts:88-90, 117-119) — identical to schema defaults, so only matters if a key is set to empty string.

Invariant (test agent:tests/rearCoreSwitch.test.ts:195-203): `REAR_RULE_SET_DIR` must lie inside `dirname(REAR_MIHOMO_CONFIG_PATH)` — mihomo only reads files under its `-d` home ("path is not subpath of home directory or SAFE_PATHS"). Orchestrator hardcodes the same dir in generated configs: `REAR_RULE_SET_DIR = '/etc/route-agent/rules'` (orch:src/core/generators/rearMihomo.ts:116, used at :344).

Invariant (test agent:tests/rearConfigRelocation.test.ts:46-57): `REAR_SINGBOX_CONFIG_PATH` default must NOT start with `/etc/sing-box/` (packaged sing-box runs with `-C /etc/sing-box`, a directory merge of all `*.json`; config.ts:26-34).

---

## 1. Rear core abstraction — agent:src/utils/rearCore.ts

Two descriptor objects implementing `RearCore` (:44-59). Differences are data, not branches (:17-20).

### 1.1 `SINGBOX_REAR_CORE` (:85-112)
- `id: 'singbox'`, `label: 'sing-box'`.
- `binaryPath`: `SINGBOX_BINARY_PATH` — **same binary as the front instance** (rearSingbox.service.ts:21-24; test agent:tests/app.test.ts:161-177).
- `configPath`: `REAR_SINGBOX_CONFIG_PATH`; `unitPath`: `REAR_SINGBOX_UNIT_FILE_PATH`; `unitName()` = `basename(unitPath, '.service')` (:61) → `route-rear-singbox`.
- `unitContent()` = `buildUnit` with:
  - `Description=Rear sing-box (WARP egress, managed by route-agent)`
  - `ExecStart=<binary> run -c <cfg>`
  - `ExecReload=/bin/sh -c "<binary> check -c <cfg> && /bin/kill -HUP $MAINPID"`
- `validate` = `validateSingBoxConfig` (agent:src/utils/singbox.ts:252-275, see §2.4).
- `warpMembers: { pool: 'wg-pool', direct: 'direct' }`.
- `clashApiFrom(parsed)` (:105-111): `parsed.experimental.clash_api.external_controller` must be a string; trimmed; empty → `null`. `secret` = `clash_api.secret` if string else `''`.

### 1.2 `MIHOMO_REAR_CORE` (:114-147)
- `id: 'mihomo'`, `label: 'mihomo'`, `binaryPath`: `MIHOMO_BINARY_PATH`, `configPath`: `REAR_MIHOMO_CONFIG_PATH`, `unitPath`: `REAR_MIHOMO_UNIT_FILE_PATH`, unit name `route-rear-mihomo`.
- `unitContent()` with `workDir = dirname(cfg)`:
  - `Description=Rear mihomo (WARP egress with rule sets, managed by route-agent)`
  - `ExecStart=<binary> -d <workDir> -f <cfg>`
  - `ExecReload=/bin/sh -c "<binary> -t -d <workDir> -f <cfg> && /bin/kill -HUP $MAINPID"`
  - Why SIGHUP not restart: "проверено на живом бинаре 2026-09-06 — mihomo перечитывает конфиг по этому сигналу, не завершаясь (`mode` сменился на лету, pid остался прежним)" (:128-132).
- `validate` = `validateMihomoConfig` (:155-175, §2.4).
- `warpMembers: { pool: 'warp-auto', direct: 'DIRECT' }` — "PUT с чужим именем mihomo отвергает" (:29-36).
- `clashApiFrom(parsed)` (:141-146): top-level `external-controller` (string, trimmed, non-empty) and top-level `secret` (string else `''`).

### 1.3 Unit template — `buildUnit` (:67-83), byte-exact

```
[Unit]
Description=${description}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${execStart}
Restart=on-failure
RestartSec=5
ExecReload=${execReload}

[Install]
WantedBy=multi-user.target
```
(trailing newline after the last line). Byte equality matters: `ensureRearUnit` compares the file text with this string (rearSingbox.service.ts:62-63); any byte difference in the Go port rewrites the unit → `unitChanged=true` → **restart** (not reload) of the rear on the first push after upgrade (§2.3 step 7).

### 1.4 Core resolution
- `resolveRearCore(coreId)` (:181-183): exactly `'mihomo'` → mihomo; anything else (incl. `''`, `'singbox'`, `'SINGBOX'`, garbage) → sing-box. Empty = proto3 default from old orchestrators (:177-180; test agent:tests/rearCoreSwitch.test.ts:76-90).
- `otherRearCore(core)` (:186-188).
- `resolveActiveRearCore()` (:198-204): `stat` mihomo config path, then sing-box config path; first that exists wins (mihomo precedence: "его юнит и поднимается последним"). `null` if neither.
- `readActiveRearClashApi()` (:212-224): active core → `readFile(configPath)` → `JSON.parse` (mihomo config is written as JSON into `.yaml`; :217-218) → `core.clashApiFrom`. Any read/parse error → `null` (silent). Returns `{ core, address, secret }`.
  - Note: parsing a **hand-written real YAML** mihomo config would fail `JSON.parse` → `null` → all consumers treat as "no rear". Go must parse as JSON (not YAML) for parity, or knowingly accept YAML superset.

Consumers of `readActiveRearClashApi`: WARP guard, GetRearConnections, GetWarpKeyHealth, PullNodeWarnings rule-provider check.

---

## 2. RPC `ConfigureRearSingbox`

Proto: `RearSingboxConfigPayload { orchestrator_secret=1; enabled=2; config_json=3; core=4 }`, `RearSingboxConfigResponse { success=1; message=2; running=3; skipped_reason=4 }` (agent:proto/agent.proto:711-741).

### 2.1 Inputs read
- `enabled` → `Boolean(...)` (rearSingbox.service.ts:189).
- `configJson` (`|| config_json || ''`) (:190).
- `core` → `resolveRearCore` (:193); `previous = otherRearCore(core)` (:194).

### 2.2 Auth
`authenticateCall` fail → `{ success:false, message:'Invalid orchestrator secret token.', running:false }` (:184-187). Test: agent:tests/app.test.ts:129-139.

### 2.3 Side effects, in order

**Branch A — `enabled == false`** (:197-212). For `target` in `[core, previous]` (requested first, then the other):
1. `stopRear(target)` (:166-170): non-test: `execAsync('systemctl disable --now <unitName>')`, error ignored.
2. `unlink(target.configPath())`, error ignored.
3. `unlink(target.unitPath())`, error ignored.
Then → `{ success:true, message:'Rear instance stopped and removed.', running:false }`.
- No `daemon-reload` after deleting unit files. Legacy `/etc/sing-box/rear.json` is NOT removed here. Rule-set dir untouched. `configJson` is not parsed (may be empty).
- Why both cores: "«тыл выключен» — это состояние узла" + configs hold WARP private keys (:198-204; test agent:tests/rearCoreSwitch.test.ts:146-167, agent:tests/app.test.ts:197-216).

**Branch B — `enabled == true`**:
1. **Binary precondition** (:214-224): `stat(core.binaryPath())`. Missing and non-test →
   `{ success:false, message:'<label> is not installed on this node — nothing to run a rear instance from.', running:false, skippedReason:'<id>_not_installed' }` — i.e. `singbox_not_installed` or `mihomo_not_installed`. (The dash is U+2014 `—`.) Nothing else touched (previous core keeps running).
2. `configObj = JSON.parse(rawConfig)` (:226). Throws on empty/invalid → caught → Internal Agent Error (step E).
3. **Core switch cleanup** (:249-260): `previous.id !== core.id` is always true (two cores), so effectively: if `stat(previous.unitPath())` exists:
   - log info; `stopRear(previous)` (`systemctl disable --now <prevUnit>`, errors ignored);
   - `unlink(previous.unitPath())`, `unlink(previous.configPath())` (errors ignored);
   - non-test: `execAsync('systemctl daemon-reload')`, error ignored.
   - Detection keys on the previous **unit file** only; a leftover previous config without unit is not removed.
   - Why before bringing up the new one: both bind loopback 29000/29001 (:246-248; test agent:tests/rearCoreSwitch.test.ts:115-144).
4. **`ensureRearUnit(core)`** (:58-75): read unit file (missing → null); if text === `core.unitContent()` → return `false`. Else `mkdir -p dirname(unitPath)`, `writeFile(unitPath, content, 'utf-8')` (non-atomic, default mode 0666&~umask → typically 0644), log, non-test `execAsync('systemctl daemon-reload')` (failure → warn only), return `true`.
5. **No-op short-circuit** (:262-270): if `!unitChanged && rearConfigMatches(core, configObj) && isRearRunning(core)` →
   `{ success:true, message:'Rear sing-box configuration already current — nothing to write or reload.', running:true }` (note: literal "sing-box" even for mihomo). No validation, no write, no reload.
   - `rearConfigMatches` (:126-129): on-disk text `=== serializeRearConfig(configObj)` where `serializeRearConfig = JSON.stringify(obj, null, 2)` (:82-84). **Comparison is on the re-serialized parsed object**, so key order and number formatting follow JS `JSON.stringify` (insertion order; numbers in JS shortest form; `-0` → `0`; large ints lose precision beyond 2^53; non-ASCII emitted raw, not `\u` escaped; `/` not escaped). A Go port must reproduce this serialization byte-for-byte (2-space indent, `": "` separator, preserved key order, no HTML escaping) or every push after upgrade will look "changed" once — acceptable only if knowingly accepted. Order-preserving JSON in Go needs a custom decoder (`map` loses order).
   - `isRearRunning(core)` (:154-164): non-test: `execAsync('systemctl is-active <unit>')`, rejection → stdout `''`; returns `stdout.trim() === 'active'`. Test seam: `REAR_TEST_INACTIVE === '1'` → false, otherwise true.
   - Why both conditions: a down rear must be restarted even with identical config (agent:tests/rearConfigUnchanged.test.ts:113-146); a changed unit must force a write (agent:tests/rearConfigRelocation.test.ts:88-120).
6. **Validate** (:272-281): `core.validate(configObj)`. Invalid →
   `{ success:false, message:'Rejected by Node Agent: invalid rear <label> syntax. Error: <error>', running: await isRearRunning(core), skippedReason:'check_failed' }`. Live config untouched (but see Risk R1 for core switches).
7. **Write** `writeRearConfig(core, configObj)` (:106-114): `mkdir -p dirname(cfg)`; tmp = `<dir>/.rear.<Date.now()>_<uuid[0:8]>.tmp`; `writeFile(tmp, serialize)`; `rename(tmp, cfg)`. Atomic replace; mode default (≈0644); tmp not cleaned on failure.
8. **`removeLegacyRearConfig()`** (:93-103): if `SINGBOX_REAR_CORE.configPath()` (sing-box path, regardless of active core) `=== '/etc/sing-box/rear.json'` → skip. Else if `/etc/sing-box/rear.json` exists → `unlink` (failure → warn) and then always log "Removed ...". Runs after the successful write ("Удаляем ПОСЛЕ успешной записи", :89-91).
9. **Start / reload** (non-test only, :286-293):
   - `unitChanged` → `execAsync('systemctl enable --now <unit>')` then `execAsync('systemctl restart <unit>')` (ExecStart changed; SIGHUP would reread the old path, :238-245).
   - else → `startAndReloadRear(core)` (:143-151): `systemctl enable --now <unit>` then `systemctl reload <unit>` — **reload, never restart** (test agent:tests/rearReload.test.ts:15-33). `runExec` is injectable for that test.
   - Any of these rejecting → caught → step E (config already written!).
10. `running = await isRearRunning(core)`; → `{ success:true, message:'Rear <label> configuration validated, applied and (re)started.', running }` (:295-301).

**E. Catch-all** (:302-306): `{ success:false, message:'Internal Agent Error: <err.message or "Unknown error">', running:false }` (no skippedReason).

### 2.4 Validators
- `validateSingBoxConfig` (agent:src/utils/singbox.ts:252-275): test → valid. Else:
  1. `fixCaddyPermissions()` (singbox.ts:32-45, dir `/var/lib/caddy` at :10): if dir exists → `execAsync('chown -R caddy:caddy /var/lib/caddy || true')`, `execAsync('chmod -R 755 /var/lib/caddy || true')`; errors → warn. (Side effect on every rear sing-box validation.)
  2. check file in **the FRONT config dir** `dirname(SINGBOX_CONFIG_PATH)` = `/etc/sing-box/.config.check_<ms>_<uuid8>.json`, contents `JSON.stringify(obj,null,2)`; `mkdir -p`.
  3. `execFileAsync(SINGBOX_BINARY_PATH, ['check','-c', checkFile])`.
  4. Error → `{ valid:false, error: err.stderr || err.message }` (NOT truncated); `finally` unlink check file.
- `validateMihomoConfig` (rearCore.ts:155-175): test → valid. Else check file `<dirname(REAR_MIHOMO_CONFIG_PATH)>/.rear.check_<ms>_<uuid8>.yaml` (JSON content, 2-space indent), `mkdir -p`; `execFileAsync(MIHOMO_BINARY_PATH, ['-t','-d', workDir, '-f', checkFile])`; error → `{ valid:false, error:(stderr || message || 'unknown error').slice(0,1000) }`; `finally` unlink. Validated in the real work dir so `rule-providers` paths resolve; temp file so a rejected config is never what a restarting process reads (:150-154).

### 2.5 Orchestrator dependence
- Call site: orch:src/core/RearSingBoxService.ts:213-216 sends `{ enabled: node.warpEnabled, configJson, core }` with `core ∈ {'singbox','mihomo'}`, configJson always a full generated config (also when disabling). Deadline: default 60 s (orch:src/core/services/grpc/GrpcConfigPusher.ts:62,126,485-503), `onError:'returnFailure'`.
- Reads `success`, `running` (`Boolean`), `message`, `skippedReason` (RearSingBoxService.ts:231-237).
- String-matches `skippedReason === 'singbox_not_installed'` and `=== 'check_failed'` (orch:src/core/services/nodeOps/actions.ts:263-277); `mihomo_not_installed` falls into the generic branch (message shown, sliced to 200 chars). `expectRunning && !running` → warn line.
- `switchCore` rolls the DB column back if `success` is false (RearSingBoxService.ts:297-326), so `success` must be false for every failed apply.
- Before a mihomo switch the orchestrator uploads the mihomo binary and all rule sets first (RearSingBoxService.ts:100-131, 180-197) because "mihomo проверяет их наличие при разборе конфига".
- Generated configs the agent relies on: Clash API at `127.0.0.1:<REAR_SINGBOX_CLASH_API_PORT>` with derived secret for both cores (orch:src/core/generators/rearMihomo.ts:383-384); selector named `warp`; sing-box members `wg-pool`/`direct` (orch:src/core/generators/rearSingbox.ts:83-95); mihomo groups `wg-pool` (load-balance) → `warp-auto` (fallback) → `warp` (select of `[warp-auto, DIRECT]`) only when keys exist (rearMihomo.ts:280-323); key tags `warp-<keyId>` (rearSingbox.ts:113-136).

### 2.6 Concurrency
No lock. Two concurrent ConfigureRearSingbox calls interleave freely (stat/unlink/write/systemctl). Single-agent assumption: the orchestrator does not serialize per node either.

---

## 3. WARP guard (background loop)

### 3.1 Timer — agent:src/services/rearWarpGuard.service.ts:67-95
- Module globals: `timer`, `running` (:21-22).
- `startRearWarpGuard()`: no-op if already started; `setInterval(tick, REAR_WARP_GUARD_INTERVAL_MS)` (default 30 s), `unref()`. **No immediate first tick** — first pass ~30 s after bind. Started only when `NODE_ENV !== 'test'` (index.ts:203-216). `stopRearWarpGuard()` exists but is never called.
- `tick` (:70-82): if `running` → skip (no overlap; a pass can hang up to 5 s per request); `guardOnce()`; any throw → `logger.debug` only ("тыл может перезапускаться").

### 3.2 `guardOnce()` (:30-65)
1. `readActiveRearClashApi()`; `null` → return `null` (no rear).
2. `GET http://<address>/proxies`, header `authorization: Bearer <secret>` only if secret non-empty, `AbortSignal.timeout(5000)`. Non-2xx → throw `clash api responded <status> for /proxies`.
3. `decideWarpSelection(payload, active.core.warpMembers)` (agent:src/utils/rearWarpGuard.ts:50-67):
   - `payload.proxies` missing / non-object / no own-or-inherited key `warp` → `null` → return (WARP not enabled or empty pool; "«нет ключей → direct» здесь было бы неверно", test agent:tests/rearWarpGuard.test.ts:73-80).
   - keys = `parseWarpKeyHealth(payload)` (§4.2 rules: tag starts with `warp-`, non-empty suffix, not `warp-auto`); alive = last history entry has numeric `delay > 0`.
   - `desired = aliveKeys > 0 ? members.pool : members.direct`; also returns `aliveKeys`, `totalKeys`.
4. `currentSelection(payload)` (rearWarpGuard.ts:70-74) = `proxies.warp.now` if string else `null`. Equal to desired → return `null` (no PUT; "лишний PUT на каждом тике").
5. `selectWarpMember(endpoint, desired)` (rearWarpGuard.ts:87-97): `PUT http://<address>/proxies/warp`, headers `{ authorization: Bearer <secret> (only if non-empty), content-type: application/json }`, body `{"name":"<desired>"}`, timeout 5 s. Non-2xx → throw `clash api responded <status> selecting <member>` (success = 204 on sing-box; test agent:tests/rearWarpGuard.test.ts:105-156).
6. `logger.warn` with selector/to/aliveKeys/totalKeys; message chosen by `desired === 'direct'` (see Risk R6). Return desired.

Purpose/why on agent: xray balancer had `fallbackTag`, sing-box `urltest` picks an arbitrary member with no history; orchestrator polls hourly; must work while orchestrator is down (rearWarpGuard.ts:8-20). Guard never edits config; reload resets selector to `default` and guard re-evaluates (:18-20).

The guard overrides any manual selection whenever it differs from `desired`.

---

## 4. Read-only rear observations

### 4.1 RPC `GetRearConnections` — agent:src/services/singboxConnections.service.ts:58-79, agent:src/utils/rearConnections.ts
- Request: `SingBoxConnectionsRequest { orchestrator_secret }`. Auth fail → `{ success:false, message:'Invalid orchestrator secret token.', connections:[] }`.
- `collectRearConnections()` (rearConnections.ts:104-120): `readActiveRearClashApi()`; null → `[]` (debug log). Else `GET http://<address>/connections`, `authorization: Bearer <secret>` only if non-empty, timeout 5 s; non-2xx → throw `rear clash api responded <status>`; body → `parseMihomoConnections`.
- `parseMihomoConnections(payload)` (rearConnections.ts:69-96): `payload.connections` not an array → `[]`. Per element (`metadata` defaults to `{}`):
  - `id` = `asString(raw.id)` (internal, not on wire)
  - `user` = `metadata.inboundUser` (always `link` — shared VLESS uuid, kept as is; test rearConnections.test.ts:66-73)
  - `sourceIp` = `metadata.sourceIP`; `destinationIp` = `metadata.destinationIP`
  - `destinationDomain` = `metadata.host || metadata.sniffHost`
  - `destinationPort` = `asNumber(metadata.destinationPort)` (mihomo sends a string)
  - `uploadBytes` = `asNumber(raw.upload)`, `downloadBytes` = `asNumber(raw.download)`
  - `network` = `metadata.network`; `inboundTag` = `metadata.inboundName`
  - `startedAtUnixMs` = `parseStart(raw.start)`: non-string/empty → 0; `Date.parse` (RFC3339 with ns → truncated to ms); NaN → 0
  - `closed` = `false` always.
  - `asString`: string else `''`. `asNumber` (:47-54): finite number; or string via JS `Number()` (note: `''`→0, `' 12 '`→12, `'0x1A'`→26, `'1e3'`→1000); non-finite → 0.
- Response: success → `{ success:true, message: connections.length === 0 ? 'Rear core is not active on this node.' : 'OK', connections }`. Exception → `{ success:false, message:<err.message>, connections:[] }` (fetch network failure message from undici is typically `fetch failed`; abort → `The operation was aborted due to timeout`-style text).
- Wire mapping (proto SingBoxConnection fields 1-11): user, source_ip, destination_ip, destination_domain, destination_port(uint32), upload_bytes(int64), download_bytes(int64), network, inbound_tag, started_at_unix_ms(int64), closed.
- Orchestrator: orch:src/core/services/grpc/GrpcConfigPusher.ts:385-403 (deadline 10 s `OBSERVATIONAL_RPC_DEADLINE_MS`, :80), consumer orch:src/zones/anomaly-traffic/services/connectionRecordsFeed.ts:292-313 — uses `success` and `connections`, logs `message` at debug only.

### 4.2 RPC `GetWarpKeyHealth` (adjacent) — agent:src/services/warpKeyHealth.service.ts:26-84
- Auth fail → `{ success:false, message:'Invalid orchestrator secret token.', entries:[] }`.
- Order:
  1. `stat(config.REAR_SINGBOX_CONFIG_PATH || '/etc/sing-box/rear.json')` — **sing-box path only**. Missing → `{ success:true, message:'Rear sing-box is not configured on this node.', entries:[], skippedReason:'rear_not_running' }` (see Risk R2).
  2. `readActiveRearClashApi()`; null → `{ success:true, message:'Rear sing-box config carries no clash_api endpoint.', entries:[], skippedReason:'rear_not_running' }`.
  3. `fetchWarpKeyHealth` (agent:src/utils/warpKeyHealth.ts:85-99): `GET /proxies`, bearer if non-empty, timeout 5 s, non-2xx → throw `clash api responded <status> for /proxies`.
  4. → `{ success:true, message:'Reported health for <n> WARP endpoint(s).', entries:[{endpointTag, alive, rttMs, measuredAtUnixMs}] }`.
  5. Any throw → `{ success:false, message:'Could not read rear clash api: <msg>', entries:[], skippedReason:'clash_api_unreachable' }`.
- `parseWarpKeyHealth` (warpKeyHealth.ts:63-83): iterate `proxies` in key order; keep tags with prefix `warp-` and length > 5, excluding set `{'warp-auto'}` (:38, :47); `parseHistory` (:49-61): non-array/empty → dead; last element; `delay` must be a finite number > 0 else dead; `at = Date.parse(time)` if string, NaN → 0. Entry `{ endpointTag: tag, alive: measured!==null, rttMs: delay or 0, measuredAtUnixMs: at or 0 }`. `rttMs` is uint32 on the wire (fractional delay would be truncated/rejected by encoder — mihomo/sing-box send ints).
- Orchestrator: `handleReport` ignores `!success` (warn) and any non-empty `skippedReason` (debug), then retires keys by `keyIdFromEndpointTag` (orch:src/core/WarpHealthWorker.ts:98-140; tag parsing orch:src/core/generators/rearSingbox.ts:134-136).

### 4.3 `PullNodeWarnings` rear contribution — agent:src/utils/rearRuleProviderWarning.ts
- `detectEmptyRearRuleProviders()` (:73-113): `readActiveRearClashApi()`; null → null. `GET http://<address>/providers/rules` with header **`Authorization: Bearer <secret>` always** (even when secret is `''` → `Bearer `), timeout **3000 ms**. Non-2xx or any throw (network/JSON) → `null`, silently.
- `emptyRuleProviders(payload)` (:49-65): payload/`providers` must be non-null objects (arrays pass the object check but have no entries with objects…); for each `[key, raw]`: skip non-object; skip if `ruleCount` not a `number`; skip if `> 0`; push `raw.name` if non-empty string else `key`. Return sorted (JS default sort = UTF-16 code-unit order). Tests: agent:tests/rearRuleProviderWarning.test.ts:27-87 (`'0'` string and `null` not counted; sorted; key fallback).
- Non-empty → warn log; return JournalWarning `{ kind:'rear_rule_provider_empty', source: core.label ('mihomo' | 'sing-box'), sample:'наборы без правил: ' + names.join(', '), occurrences: names.length, lastSeenUnixMs: Date.now() }` (:101-112).
- Placement in response: `[notListening, emptyProviders, ...journal]`, nulls filtered (nodeWarnings.service.ts:45-56). Orchestrator dedups on `kind`, title table orch:src/core/NodeWarningWorker.ts:52.

---

## 5. mihomo uploads — agent:src/services/mihomo.service.ts

### 5.1 Shared receiver — agent:src/services/binaryReceiver.ts:55-148
- tempPath `/tmp/<tempPrefix>_<Date.now()>_<uuid[0:8]>.tmp`.
- `secretVerified = verifySecret(metadata)`. On each `data`: if not verified, try `data.orchestratorSecret || data.orchestrator_secret`; still not verified → `isAborted=true`, warn `Unauthorized <rpc> attempt rejected`, `callback(null,{success:false,message:'Invalid orchestrator secret token.'})`, `call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'))`. (So the FIRST message must authenticate if metadata didn't.)
- `version` = last non-empty `data.version` (default `'unknown'`); `targetBinary` = last non-empty `targetBinary`.
- Non-empty `chunk` → lazily `createWriteStream(tempPath)` (mode default), `write` (backpressure ignored), count bytes.
- On `end`: if aborted return; await stream end; (re-check auth, same rejection); `bytesWritten === 0` → unlink, `{ success:false, message:'No binary data received.' }`; else `apply(...)`, exceptions → `{ success:false, message:'Failed to upload binary: <msg>' }`; **unlink temp BEFORE callback** (:127-139); callback(result).
- `error` → log + cleanup; `cancelled` → cleanup (end stream, unlink).
- `is_final` is ignored; completion is the client half-close.

### 5.2 `UploadMihomoBinary` (:58-120), tempPrefix `mihomo`
apply:
1. `chmod(temp, 0o755)`.
2. non-test: `execFileAsync(temp, ['-v'])` (no timeout). Fail → `{ success:false, message:'Полученный бинарь mihomo не запускается: ' + msg.slice(0,300) }` (catches corrupt upload / SIGILL microarch, :71-78).
3. non-test: `verifyMihomoAcceptsLiveConfig(temp)` (:38-53): `configPath = config.REAR_MIHOMO_CONFIG_PATH` (no `||` fallback here); missing → ok. Else `execFileAsync(temp, ['-t','-d', dirname(configPath), '-f', configPath])`; fail → `{ ok:false, configPath, error:(stderr||message||'unknown error').slice(0,1000) }` → response `{ success:false, message:'Бинарь mihomo <version> отвергнут: он не принимает уже применённый конфиг <configPath>. Прежний бинарь оставлен на месте. ' + error.slice(0,400) }`. Binary NOT replaced.
4. `replaceBinaryAtomically(temp, MIHOMO_BINARY_PATH)` (agent:src/services/binary.service.ts:56-76): `mkdir -p` target dir; copy to `<dir>/.<basename>_<ms>_<uuid[0:6]>.tmp`; `chmod 0755`; `rename` over target; finally unlink tmp. No backup of old binary.
5. non-test: if `stat(REAR_MIHOMO_UNIT_FILE_PATH)` exists → `execAsync('systemctl restart route-rear-mihomo')` — **unit name hardcoded**, not derived from config (Risk R8). Failure → warn only. Unit is NOT created here (:99-101).
6. → `{ success:true, message:'mihomo binary version <version> successfully updated' }`.
- No capabilities (no setcap) — rear binds only `127.0.0.1:29000/29001` (:20-23).

### 5.3 `UploadRearRuleSet` (:137-203), tempPrefix `ruleset`
apply:
1. `name = targetBinary`. Must match `/^[A-Za-z0-9][A-Za-z0-9._!-]{0,63}$/` (:130; `!` allowed for `category-ai-!cn`). Fail → warn (name sliced 120) and `{ success:false, message:'Недопустимое имя набора правил.' }`.
2. `mkdir -p REAR_RULE_SET_DIR`; `destPath = <dir>/<name>.mrs`. Second guard: `dirname(resolve(destPath)) !== resolve(dir)` → error log + same message (:156-161).
3. staging `<dir>/.<name>.mrs.<Date.now()>.tmp`: `copyFile(temp, staging)`, `chmod 0644`, `rename(staging, dest)`; finally unlink staging (:165-172). Content not validated.
4. non-test (:187-198): `unit = basename(REAR_MIHOMO_UNIT_FILE_PATH, '.service')`; `execAsync('systemctl is-active <unit>')` → active iff stdout.trim()==='active' (reject → false). If active → `execAsync('systemctl reload <unit>')` (failure warn). One reload per uploaded file (orchestrator sends ~7 sets sequentially).
5. → `{ success:true, message:'Набор правил <name> записан (<bytes> байт)' }`.
- Orchestrator only checks `success` and embeds `message` in its own failure text (orch:src/core/RearSingBoxService.ts:108-111, 128-131).

---

## 6. Olcrtc — agent:src/services/olcrtc.service.ts

Architecture (:15-20): one systemd template instance per (user, node): `olcrtc-agent-srv@<user_short_uuid>.service`, config `<OLCRTC_AGENT_SRV_CONFIG_DIR>/<user_short_uuid>.yaml`. No HTTP admin port.

### 6.1 Helpers
- `sanitizeConfigInput(v)` (:22-25): `undefined|null` → `''`; else `String(v)`, remove all `\r` and `\n`, `trim()`.
- `isSafeInstanceName(v)` (:31-33): `/^[A-Za-z0-9_-]{1,128}$/`.
- `instanceConfigPath(u)` = `join(OLCRTC_AGENT_SRV_CONFIG_DIR, u + '.yaml')`; `instanceUnitName(u)` = `olcrtc-agent-srv@<u>` (no `.service`).
- `buildInstanceYaml(payload)` (:50-89) — byte-exact output (contentChanged decides restart):
  ```
  mode: srv
  auth:
    provider: <carrier>
  room:
    id: "<roomId>"
  crypto:
    key: "<cryptoKeyHex>"
  net:
    transport: <transport>
    dns: "<dns or 8.8.8.8:53>"
  ```
  then, only if `transport === 'vp8channel'` and (`vp8Fps` or `vp8BatchSize` truthy):
  ```
  vp8:
    fps: <Number(vp8Fps)|0>          (line only if vp8Fps truthy)
    batch_size: <Number(vp8BatchSize)|0>   (only if truthy)
  ```
  then, only if `transport === 'seichannel'` and any of `seiFps, seiBatchSize, seiFragmentSize, seiAckTimeoutMs` truthy:
  ```
  sei:
    fps: …            (if seiFps)
    batch_size: …     (if seiBatchSize)
    fragment_size: …  (if seiFragmentSize)
    ack_timeout_ms: … (if seiAckTimeoutMs)
  ```
  All string inputs sanitized; `dns` empty after sanitize → `8.8.8.8:53`. Every line ends with `\n`. Indentation 2 spaces. Values NOT YAML-escaped (quotes/backslashes inside roomId/key break the scalar; Risk R10). `|0` = ToInt32 (uint32 > 2^31-1 wraps negative). Numbers are read raw (not sanitized). Proto defaults 0 ⇒ "omitted".

### 6.2 `ensureOlcrtcAgentSrvSystemdUnit(overridePath?)` (:97-145)
- Path = override or `OLCRTC_AGENT_SRV_UNIT_FILE_PATH`. Expected content (byte-exact):
  ```
  [Unit]
  Description=olcrtc-agent-srv instance %i (managed by route-agent)
  After=network-online.target
  Wants=network-online.target

  [Service]
  Type=simple
  User=root
  ExecStart=<OLCRTC_AGENT_SRV_BINARY_PATH> <OLCRTC_AGENT_SRV_CONFIG_DIR>/%i.yaml
  Restart=always
  RestartSec=5

  [Install]
  WantedBy=multi-user.target
  ```
- Equal → debug, return `false`. Else `mkdir -p`, `writeFile` (non-atomic), info log, non-test `systemctl daemon-reload` (fail → warn), return `true`. Any other error → error log, return `false` (never throws).
- Called: at every agent boot before bind (index.ts:121, regardless of whether olcrtc is installed), on every SyncOlcrtcInstance, after every UploadOlcrtcAgentSrvBinary. Test: agent:tests/app.test.ts:558-575.

### 6.3 RPC `SyncOlcrtcInstance` (:152-218)
Proto: `OlcrtcInstanceConfigPayload` fields 1-13 (agent:proto/agent.proto:301-317); response `OlcrtcInstanceConfigResponse { success, message }`.
1. Auth (metadata only) fail → `{ success:false, message:'Invalid orchestrator secret token.' }`.
2. `userShortUuid = sanitizeConfigInput(request.userShortUuid)`; not safe → warn, `{ success:false, message:'Invalid or missing user_short_uuid.' }` (test app.test.ts:951-960 with `../../etc/passwd`).
3. `ensureOlcrtcAgentSrvSystemdUnit()`.
4. `yaml = buildInstanceYaml(request)`; `existing = readFile(configPath)` (missing → null); `contentChanged = existing !== yaml`.
5. `mkdir -p dirname(configPath)`; `writeFile(configPath, yaml)` — **always written** (even unchanged), non-atomic, default mode.
6. non-test:
   - `execAsync('systemctl enable olcrtc-agent-srv@<u>')` — failure → warn, continue.
   - changed → `execAsync('systemctl restart olcrtc-agent-srv@<u>')`; failure → `msg = (err.stderr || err.stdout || err.message || 'systemctl restart error').trim()` → `{ success:false, message:'Failed to start olcrtc instance (olcrtc-agent-srv@<u>): <msg>' }`.
   - unchanged → `execAsync('systemctl start olcrtc-agent-srv@<u>')`; failure → debug only.
7. → `{ success:true, message:'olcrtc instance <u> configured (restarted|unchanged).' }` (word chosen by `contentChanged`, even in test where nothing ran).
8. Catch-all → `{ success:false, message:'Olcrtc instance sync error: <msg>' }`.
- Orchestrator (orch:src/core/services/orchestrator/systemOrchestrator.ts:795-814): sends `userShortUuid, carrier, transport, roomId, cryptoKeyHex, dns` and `vp8Fps/vp8BatchSize` only for vp8channel; `!success` → throws `Error(message)`; then `ensureOlcrtcEventStream`. Default 60 s deadline (GrpcConfigPusher.ts:558-580). Test: app.test.ts:962-989.

### 6.4 RPC `DeleteOlcrtcInstance` (:225-260)
1. Auth (metadata only) / name validation — same messages as Sync.
2. non-test: `execAsync('systemctl disable --now olcrtc-agent-srv@<u> || true')` (shell `|| true`; catch → warn).
3. `unlink(configPath)`, error ignored. No daemon-reload; template unit file kept.
4. → `{ success:true, message:'olcrtc instance <u> removed.' }`; catch-all `{ success:false, message:'Olcrtc instance delete error: <msg>' }`.
- Orchestrator: `!success` → throw/log; on success deletes its DB row (systemOrchestrator.ts:839-851). Test: app.test.ts:1002-1023.

### 6.5 RPC `StreamOlcrtcEvents` (:271-356)
Proto: `OlcrtcEventsRequest { orchestrator_secret }` → `stream OlcrtcEvent` (agent:proto/agent.proto:329-350).
1. Auth (`authenticateCall`) fail → warn; `err = new Error('UNAUTHENTICATED: Invalid orchestrator secret token.')`, `err.code = 16`; `call.emit('error', err)` → client gets status `UNAUTHENTICATED`, details = that message; return. Test: app.test.ts:1025-1043 (expects `error`, no `data`, no clean `end`).
2. info log; `spawn('journalctl', ['-u', 'olcrtc-agent-srv@*', '-n', '0', '-f', '--output', 'json'])` — no shell; glob interpreted by journalctl; `-n 0` = **no backlog**, only live entries. stdin/stderr pipes default (stderr never read).
3. `cleanup()` (idempotent via `isCleanedUp`): `journalProcess.kill('SIGTERM')` if not killed and has pid; info log. Bound to call events `cancelled`, `close`, `finish`, `error`.
4. `journalProcess.on('error')` → warn only (e.g. ENOENT). **Does not end or error the gRPC stream.**
5. stdout `error` → debug. stdout `data`: if cleaned up or `call.destroyed` → ignore chunk; append `chunk.toString()` (UTF-8 per chunk) to `lineBuffer`; split on `\n`; skip blank lines; per line, inside try (any exception → skip line silently):
   - `entry = JSON.parse(line)`; `unit = String(entry._SYSTEMD_UNIT || '')`; must match `/^olcrtc-agent-srv@(.+)\.service$/` → `userShortUuid = match[1]`.
   - `entry.MESSAGE` must be a non-empty string (journald emits byte arrays for non-UTF-8 — skipped).
   - `event = JSON.parse(MESSAGE)` (non-JSON stderr lines skipped).
   - re-check cleaned/destroyed → return.
   - `call.write({ userShortUuid, type: event.type||'', timestamp: event.ts ? (Date.parse(event.ts) || Date.now()) : Date.now(), sessionId: event.session_id||'', deviceId: event.device_id||'', reason: event.reason||'', addr: event.addr||'', bytesIn: event.bytes_in||0, bytesOut: event.bytes_out||0, healthJson: event.health ? JSON.stringify(event.health) : '' })`. `timestamp` = Unix ms. Write return value (backpressure) ignored.
6. Stream lifecycle: never ended by the server. It lives until the client cancels/disconnects. If journalctl dies or never starts, the stream stays open and silent (Risk R3). Each open stream spawns its own journalctl.
- Orchestrator consumer: orch:src/core/services/grpc/GrpcOlcrtcEventStreamManager.ts — opens with metadata + payload secret (:84), reconnect ladder 10 s / 30 s / 5 min only on `error`/`end` (:46, :114-125), resets ladder on each `data` (:105). Dispatch by `type` exact strings `session_open`, `traffic`, `session_close`, `health`; unknown → debug (:142-203). Uses `userShortUuid`, `sessionId`, `deviceId`, `addr`, `bytesIn`, `bytesOut`, `reason`, `healthJson` (stored raw, `|| null`), `timestamp`.

### 6.6 RPC `UploadOlcrtcAgentSrvBinary` (:363-490) — its own copy of the receiver
- Auth/chunk handling identical in behavior to §5.1 (temp `/tmp/olcrtc-agent-srv_<ms>_<uuid8>.tmp`), messages identical, except error/cancel cleanup does NOT await.
- On end (authed, bytes>0): `mkdir -p /tmp` (dirname of temp), `chmod 0755`, `replaceBinaryAtomically(temp, OLCRTC_AGENT_SRV_BINARY_PATH)`, unlink temp; `unitJustProvisioned = ensureOlcrtcAgentSrvSystemdUnit()`.
- non-test and `!unitJustProvisioned`: `execAsync("systemctl list-units --type=service --state=running --no-legend 'olcrtc-agent-srv@*' | awk '{print $1}'")`; for each non-empty trimmed line, sequentially `execAsync('systemctl restart <unit>')` (failure → warn per unit); enumerate failure → warn.
- → `{ success:true, message:'olcrtc-agent-srv binary version <version> successfully updated' }`; errors → `{ success:false, message:'Failed to upload binary: <msg>' }` (temp unlinked). No smoke test (`-v`) and no config acceptance check, unlike mihomo. Test: app.test.ts:476-506.

Related (other group): telemetry `getWebRtcStatus` uses the same `list-units` pipeline to report `not_installed | no_active_tunnels | nominal | unavailable` (agent:src/utils/telemetry.ts:77-97).

---

## 7. Background timers / loops in scope

| Loop | Interval | Start | Overlap | Errors |
|---|---|---|---|---|
| Rear WARP guard (§3) | `REAR_WARP_GUARD_INTERVAL_MS` = 30 s, `setInterval`, unref | after gRPC bind, non-test | `running` flag skip | debug log |
| StreamOlcrtcEvents journal tail (§6.5) | event-driven per open stream | per RPC | one `journalctl -f` per stream | warn/debug, stream not closed |
| Boot olcrtc unit provisioning | once | `startServer()` before bind | — | logged, never fatal |

No retries anywhere in scope (single attempt per call; the orchestrator retries by re-pushing).

---

## 8. Parity checklist (testable)

ConfigureRearSingbox
1. Bad secret (metadata and payload both wrong) → `success=false`, `message='Invalid orchestrator secret token.'`, `running=false`, no FS changes. Payload-only correct secret is accepted.
2. `core` `''`/absent/`'singbox'`/any non-`mihomo` → sing-box paths; `'mihomo'` → mihomo paths only (no sing-box config/unit created).
3. `enabled=false` with any core → both cores: `systemctl disable --now` each, both configs and both unit files deleted, `success=true`, `running=false`, `message='Rear instance stopped and removed.'`; empty `configJson` accepted.
4. Missing binary (prod) → `skippedReason='<core>_not_installed'`, exact message with U+2014, nothing stopped or written.
5. Invalid JSON with `enabled=true` → `success=false`, `message` starts `Internal Agent Error: `, `running=false`, no skippedReason.
6. Previous core's unit present → previous stopped/disabled, its unit+config deleted, `daemon-reload`, before validation of the new config.
7. Unit file text byte-identical to template in §1.3 for each core (ExecStart/ExecReload exact, incl. quotes and `$MAINPID`).
8. Unit unchanged + config text equal to `JSON.stringify(parsed,null,2)` + `systemctl is-active` = `active` → `success=true`, `running=true`, message contains `already current`, file mtime unchanged, no `check`, no systemctl reload.
9. Same config but rear inactive → validate + rewrite + reload; message lacks `already current`.
10. Unit changed (e.g. stale file) → rewrite unit, `daemon-reload`, write config, `enable --now` + `restart` (not reload).
11. Normal change → `enable --now` then `reload` (never `restart`).
12. Validation failure → `skippedReason='check_failed'`, message `Rejected by Node Agent: invalid rear <label> syntax. Error: <stderr>`; config file untouched; `running` reflects `is-active`.
13. sing-box check uses `<sing-box> check -c /etc/sing-box/.config.check_*.json`; mihomo uses `<mihomo> -t -d <workdir> -f <workdir>/.rear.check_*.yaml`; temp removed in all cases; mihomo error truncated to 1000 chars, sing-box not truncated.
14. Config write is tmp+rename in the target dir; written content = 2-space JSON of parsed object, insertion order preserved.
15. After a successful write, `/etc/sing-box/rear.json` is deleted if present (only when the sing-box rear path is not itself that path).
16. Success message `Rear <label> configuration validated, applied and (re)started.`; `running` from `systemctl is-active`.
17. systemctl failure during start/reload → `Internal Agent Error: ...`, `running=false`, config stays written.

Clash API discovery & guard
18. Active core = mihomo if `REAR_MIHOMO_CONFIG_PATH` exists, else sing-box if its config exists, else none; config parsed as JSON; mihomo reads top-level `external-controller`/`secret`, sing-box reads `experimental.clash_api.external_controller`/`secret`; empty/whitespace address → none; parse error → none.
19. Guard: no first tick at start; every 30 s; skips while previous pass runs; `GET /proxies` with bearer only if secret non-empty, 5 s timeout.
20. No `proxies.warp` → no action. Key = tag `warp-` + non-empty suffix, not `warp-auto`; alive iff last history entry has numeric delay > 0. Group tags `wg-pool`/`warp` never count.
21. desired = pool member (`wg-pool` | `warp-auto`) if ≥1 alive key else direct (`direct` | `DIRECT`); PUT `/proxies/warp` with `{"name":desired}` and `content-type: application/json` only when `proxies.warp.now !== desired`; non-2xx = error (logged at debug).

GetRearConnections / GetWarpKeyHealth / warnings
22. No active rear → `success=true`, `connections=[]`, message `Rear core is not active on this node.`; active rear with 0 connections → same message.
23. Field mapping exactly as §4.1 (host over sniffHost; string port → number; `start` RFC3339 → ms; `closed=false`; `user=inboundUser`).
24. Non-2xx → `success=false`, message `rear clash api responded <status>`, empty list.
25. GetWarpKeyHealth: sing-box rear config absent → `rear_not_running` even if a mihomo rear runs (current behavior); API down → `success=false`, `skippedReason='clash_api_unreachable'`, message prefix `Could not read rear clash api: `.
26. Rule providers: `GET /providers/rules`, `Authorization: Bearer <secret>` always, 3 s timeout; any failure → no warning; names with numeric `ruleCount <= 0` (incl. negative), sorted, name falls back to key; warning fields exactly as §4.3.

mihomo uploads
27. Unauthed first message (no metadata secret) → `Invalid orchestrator secret token.` + stream destroyed; zero bytes → `No binary data received.`; temp file deleted before the response.
28. mihomo binary: `-v` smoke test; live-config acceptance with `-t -d <dir> -f <cfg>` only if config exists; rejection messages exact (§5.2), binary not replaced; replace via same-dir tmp + rename, 0755; restart `route-rear-mihomo` only if unit file exists.
29. Rule set name regex `^[A-Za-z0-9][A-Za-z0-9._!-]{0,63}$`; `../x` rejected; file `<dir>/<name>.mrs` mode 0644 via staging+rename; reload mihomo unit only if `is-active` = active; message `Набор правил <name> записан (<bytes> байт)`.

Olcrtc
30. Sync/Delete reject payload-only secret (metadata required).
31. `user_short_uuid` sanitized (CR/LF stripped, trimmed) then must match `^[A-Za-z0-9_-]{1,128}$`, else `Invalid or missing user_short_uuid.`.
32. YAML byte-exact per §6.1 incl. vp8/sei blocks only for matching transport and non-zero values; default dns `8.8.8.8:53`.
33. Sync always rewrites the YAML; `enable` always; `restart` iff content changed (failure → `success=false`, `Failed to start olcrtc instance (olcrtc-agent-srv@<u>): <stderr|stdout|message trimmed>`); else `start` (failure ignored); message `... configured (restarted).` / `(unchanged).`.
34. Template unit byte-exact per §6.2; rewritten + daemon-reload only on difference; provisioned at boot.
35. Delete: `systemctl disable --now olcrtc-agent-srv@<u> || true` via shell, unlink YAML, `success=true` even if nothing existed.
36. Stream: unauth → gRPC status 16 UNAUTHENTICATED, details `UNAUTHENTICATED: Invalid orchestrator secret token.`; spawns `journalctl -u olcrtc-agent-srv@* -n 0 -f --output json`; only lines whose `_SYSTEMD_UNIT` matches the instance pattern and whose MESSAGE is JSON produce events; field mapping per §6.5 (timestamp fallback to now; `healthJson` = compact JSON of `health` or `''`); malformed lines skipped without ending the stream; journalctl SIGTERMed on client cancel/close.
37. Olcrtc binary upload: replace atomically; if the template unit was already current, restart every running `olcrtc-agent-srv@*` unit; if it was just (re)written, restart none.

---

## 9. Risks / smells (flag only)

- **R1 — core switch is not "check before touch".** On a switch, the previous core is stopped and its config+unit deleted (rearSingbox.service.ts:249-260) BEFORE `core.validate` (:272). A `check_failed` (or a later systemctl failure) leaves the node with **no rear at all**, contradicting the handler's own doc ("прежний конфиг остаётся на месте нетронутым", :175-178) and proto comment (agent.proto:737-739). Orchestrator rolls its DB column back and the next push re-creates the old core, but the interval is a full outage of the WARP branch.
- **R2 — GetWarpKeyHealth is blind on mihomo nodes.** It pre-checks `REAR_SINGBOX_CONFIG_PATH` existence (warpKeyHealth.service.ts:35-46); after a switch to mihomo that file is deleted, so the RPC always answers `rear_not_running` and the orchestrator (WarpHealthWorker.ts:103-107) never measures/retires mihomo keys. The `rearCoreClashApi` test header describes exactly this class of silent failure as fixed (tests/rearCoreClashApi.test.ts:13-20) but the pre-check survived. Also messages say "sing-box" for mihomo.
- **R3 — StreamOlcrtcEvents can hang silently.** journalctl spawn error / exit never ends or errors the gRPC call (olcrtc.service.ts:304-306; no `exit` handler). The orchestrator reconnects only on `error`/`end`, so the node's event feed stays dead until the TCP connection drops. Also: `-n 0` loses everything emitted between reconnects; stderr pipe never drained (could block journalctl if it writes a lot); `chunk.toString()` may split multi-byte UTF-8 across chunks; write backpressure ignored; one journalctl per concurrent stream, no cap.
- **R4 — secrets written world-readable.** Rear configs (WARP private keys), mihomo check temp files, and olcrtc YAML (`crypto.key`) are written with default mode (0666 & ~umask, typically 0644). Olcrtc YAML write is non-atomic.
- **R5 — sing-box rear validation writes into `/etc/sing-box/`.** `validateSingBoxConfig` places the rear config temporarily at `/etc/sing-box/.config.check_*.json` (singbox.ts:257-264) — the very directory the relocation was meant to keep rear content out of (config.ts:26-34); a packaged `sing-box -C /etc/sing-box` starting in that window could merge it (depends on whether sing-box's `-C` includes dotfiles — undetermined). It also runs `chown/chmod -R /var/lib/caddy` as a side effect of every rear validation.
- **R6 — wrong guard log text on mihomo.** `decision.desired === 'direct'` (rearWarpGuard.service.ts:60) never matches `DIRECT`, so a mihomo fallback-to-direct is logged as "restored: live keys are back".
- **R7 — "already current" message hardcodes "sing-box"** for the mihomo core (rearSingbox.service.ts:267). Tests match only `/already current/`.
- **R8 — hardcoded unit name** `systemctl restart route-rear-mihomo` in the mihomo upload (mihomo.service.ts:109) ignores `REAR_MIHOMO_UNIT_FILE_PATH`, while the rule-set path derives the name from config (:188).
- **R9 — no timeouts on any exec** (systemctl, `sing-box check`, `mihomo -t`, `mihomo -v`, `journalctl`); a hung command hangs the RPC until the orchestrator deadline (60 s default / 10 min for binaries), and the guard's `running` flag would block forever only if fetch hung (fetch has 5 s timeout, so no).
- **R10 — YAML injection / breakage in olcrtc config.** `carrier`/`transport` unquoted, `roomId`/`key`/`dns` double-quoted without escaping `"`/`\` (olcrtc.service.ts:57-66). Only CR/LF stripped. Input is orchestrator-generated, so risk is malformed config rather than attack.
- **R11 — config persisted before a failed reload.** If `systemctl enable --now`/`reload`/`restart` fails after `writeRearConfig`, the new config is on disk while the process may still run the old one; the next identical push short-circuits to "already current" if the unit is active (rearSingbox.service.ts:263). For sing-box the reload's own `check` failing makes `systemctl reload` exit non-zero → this path.
- **R12 — first enable double-loads.** `enable --now` (start with new config) immediately followed by `reload` (SIGHUP) on first provisioning → an extra full instance recreation on sing-box.
- **R13 — leftovers.** Disable path: no `daemon-reload` after deleting units, legacy `/etc/sing-box/rear.json` and rule-set dir kept. Switch path: previous config without unit file is not removed, and `resolveActiveRearCore` prefers mihomo if `rear.yaml` lingers. `writeRearConfig` tmp file not removed on failed rename. `removeLegacyRearConfig` logs "Removed" even after a failed unlink.
- **R14 — no concurrency control** for ConfigureRearSingbox, Sync/DeleteOlcrtcInstance, rule-set uploads (each triggers its own reload).
- **R15 — duplicated receiver.** `UploadOlcrtcAgentSrvBinary` re-implements `receiveStreamedBinary` (binaryReceiver.ts:25-28 acknowledges it); lacks the mihomo-style `-v` smoke test; when the template unit was just rewritten, running instances are NOT restarted and keep the old binary. Both receivers ignore write backpressure and `createWriteStream` errors.
- **R16 — journal warnings ignore the mihomo unit.** `managedUnits()` lists `route-rear-singbox` but not `route-rear-mihomo` (agent:src/utils/journalWarnings.ts:218-224).
- **R17 — GetRearConnections assumes mihomo shape** even when the active rear is sing-box (no `inboundName`/`inboundUser` there → empty fields); the "not active" message is also returned for an active rear with zero connections.
- **R18 — proto comments drift from code.** `SyncOlcrtcInstance` proto says "same shape write + restart every time" (agent.proto:18-19) but the agent restarts only on change; `skipped_reason` doc lists only `singbox_not_installed`/`check_failed` but `mihomo_not_installed` exists.
- **R19 — JSON number/order fidelity.** The "unchanged" comparison depends on JS `JSON.parse`→`JSON.stringify` round-trip semantics (precision loss > 2^53, key order). A Go port with different serialization changes skip/restart behavior once per node after upgrade.

## 10. Undetermined
- Whether `sing-box -C <dir>` includes dot-prefixed `*.json` files (R5 severity).
- Exact gRPC behavior when `call.write` receives a non-integer/invalid `bytes_in` (e.g. float or negative from olcrtc) — proto-loader serialization may throw inside the try (line skipped) or error the call; not tested.
- grpc-js status details for the unauthenticated stream: emitted Error message is `UNAUTHENTICATED: Invalid orchestrator secret token.`; whether grpc-js prefixes anything further on the client was not verified (orchestrator only logs it).
- Whether sing-box's Clash API serves `/providers/rules` (affects rule-provider warning on sing-box rear; failure is silent either way).
