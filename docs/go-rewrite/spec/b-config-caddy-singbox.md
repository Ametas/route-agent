# Group B: configuration apply (sing-box, Caddy, Clash API observation)

Behavioral spec of the TypeScript `route-agent` (branch `go-rewrite`), written so the agent can be
ported to Go without losing behavior. Every claim cites `file:line` in `/home/user/route-agent`
unless it says `orch:`, which means `/home/user/vpn-orchestrate`.

Scope:

- RPCs: `ApplyConfig`, `ConfigureCaddy`, `GetSingBoxUserTraffic`, `GetSingBoxConnections`.
  `GetManagedCertificate` is covered in brief because it reads the cert layout that `ConfigureCaddy`
  writes. `ConfigureAwg` lives in the same file and gets a short summary in §9. It is expected to be
  specified in full by the AWG group.
- Modules: `src/services/config.service.ts`, `src/utils/singbox.ts`, `src/utils/caddy.ts`,
  `src/utils/hotUsers.ts`, `src/services/singboxStats.service.ts`, `src/utils/singboxStats.ts`,
  `src/services/singboxConnections.service.ts`, `src/utils/singboxConnections.ts`,
  `src/utils/listenerWarning.ts`, plus the helpers they depend on: `src/utils/plannedWrites.ts`,
  `src/utils/certStorage.ts`, `src/utils/firewall.ts` (`syncEgressFirewall`), `src/middleware/auth.ts`,
  `src/utils/exec.ts`, and `getSingBoxVersion` in `src/utils/telemetry.ts`.

---

## 0. Cross-cutting conventions

### 0.1 Proto loading and field naming

- The server loads the proto with `keepCase: false, longs: String, enums: String, defaults: true, oneofs: true`
  (`src/index.ts:51-57`). Request fields therefore arrive **camelCase** (`configJson`, `forceReload`,
  `caddyfileContent`, `extraCertificates[].certPem`). Absent scalars arrive as defaults (`''`, `false`),
  and absent repeated fields arrive as `[]`.
- Every handler also reads the snake_case spelling (`call.request.config_json`, `camouflage_path`, …). Under
  `keepCase:false` these are **always undefined**, so they are dead code. A Go port reads the proto
  fields directly.
- Response objects are built as camelCase JS objects. An unknown or snake_case key is **silently
  dropped** by proto-loader. `tests/responseFieldNaming.test.ts:1-85` guards this. A Go port that uses
  generated structs has no such trap.
- Extra keys that are not in the schema are dropped on serialization. For example,
  `GetSingBoxConnections` returns records that carry an internal `id` (`src/utils/singboxConnections.ts:25-32`),
  and that `id` never reaches the wire.
- The orchestrator also loads with `keepCase:false, longs: String` (orch:`src/core/services/grpc/GrpcConnectionManager.ts:17-23`).
  As a result, **int64 response fields reach the orchestrator as decimal strings** (see §8 risk R-O1).

### 0.2 Authentication

- `verifySecret(token)` (`src/middleware/auth.ts:7-19`): `false` if the token is empty or not a string.
  `false` if `EGRESS_CONTROL_SECRET` is empty or the byte lengths differ. Otherwise the result is
  `crypto.timingSafeEqual` over the UTF-8 bytes.
- `extractSecretFromMetadata(call)` (`auth.ts:23-26`): the **first** value of the metadata key
  `x-orchestrator-secret`, stringified, or `''`.
- `authenticateCall(call)` (`auth.ts:31-39`): the metadata secret first, then the fallback
  `call.request.orchestratorSecret || call.request.orchestrator_secret`.
- Per RPC:
  | RPC | Check used | Payload fallback effective? |
  |---|---|---|
  | ApplyConfig | `verifySecret(extractSecretFromMetadata)`, metadata only (`config.service.ts:103-108`) | n/a: `ConfigPayload` has no secret field |
  | ConfigureCaddy | `authenticateCall` (`config.service.ts:242-245`) | **No**: `CaddyConfigPayload` has no `orchestrator_secret` field, so the loader drops it |
  | GetSingBoxUserTraffic | `authenticateCall` (`singboxStats.service.ts:18-21`) | Yes: `SingBoxUserTrafficRequest.orchestrator_secret = 1` |
  | GetSingBoxConnections | `authenticateCall` (`singboxConnections.service.ts:25-28`) | Yes: `SingBoxConnectionsRequest.orchestrator_secret = 1` |
- On auth failure, every handler returns **gRPC status OK** with `success:false`, the message
  `'Invalid orchestrator secret token.'`, and empty repeated fields where the response has any. It never
  returns a gRPC error status. Tested at `tests/app.test.ts:111-123` (ApplyConfig, exact message),
  `:900-909` (Caddy), and `:1331-1342` (traffic, exact message, empty entries).
- The orchestrator always sends the secret in metadata (orch:`GrpcConnectionManager.ts:329-333`) and sends
  `{}` as the request body for both observation RPCs (orch:`GrpcConfigPusher.ts:357,411`).

### 0.3 Error posture

- None of the four handlers ever fails at the gRPC level. Every failure is `callback(null, {success:false, message})`.
  The orchestrator wraps true transport errors as `gRPC Failure [Code N]: …` (orch:`GrpcConfigPusher.ts:192-205`).
  A Go port must keep returning status OK with `success=false`. Otherwise the orchestrator sees a different
  message shape, and for ApplyConfig (`onError:'throw'`) a thrown error instead of a result.
- No handler has a process-level guard. There is no `unhandledRejection` handler anywhere in `src/`. All
  awaited work sits inside `try`, so this is only theoretical.

### 0.4 Exec semantics (`src/utils/exec.ts:1-5`)

- `execAsync = promisify(child_process.exec)` runs through `/bin/sh -c`, with **no timeout** and the default
  `maxBuffer` (1 MiB). On a non-zero exit it rejects with an `Error` whose `message` is
  `"Command failed: <cmd>\n<stderr>"`, and which carries `.stdout`, `.stderr`, and `.code`.
- `execFileAsync = promisify(child_process.execFile)` runs with no shell, no timeout, and the same rejection
  shape. When the binary is missing, it rejects with `spawn <path> ENOENT`, where `.stderr` is undefined.
- **No external command in this group has a timeout.** The only bound is the orchestrator's RPC deadline
  (see §0.6). A deadline does not cancel the agent-side work, which keeps running after the client gives up.

### 0.5 Configuration (`src/config.ts`)

| Env | Default | Used by |
|---|---|---|
| `SINGBOX_CONFIG_PATH` | `/etc/sing-box/config.json` (`config.ts:10`) | ApplyConfig, clash endpoint, listener check |
| `SINGBOX_BINARY_PATH` | `/usr/local/bin/sing-box` (`:11`) | `sing-box check` / `version` |
| `SINGBOX_UNIT_FILE_PATH` | `/etc/systemd/system/sing-box.service` (`:12`) | version gate, unit provisioning |
| `RELOAD_COMMAND` | `systemctl reload sing-box` (`:13`) | sing-box apply |
| `SINGBOX_RESTART_COMMAND` | `systemctl restart sing-box` (`:25`) | stale-image apply |
| `CADDYFILE_PATH` | `/etc/caddy/Caddyfile` (`:58`) | ConfigureCaddy |
| `CADDY_RELOAD_COMMAND` | `systemctl reload caddy \|\| systemctl restart caddy` (`:68`) | ConfigureCaddy |
| `CADDY_BINARY_PATH` | *(unset)* (`:72`) | `resolveCaddyBinary`, systemd override |
| `EGRESS_CONTROL_SECRET` | required (`:9`) | auth |

The `CADDY_RELOAD_COMMAND` default **must** include `|| systemctl restart caddy`. `reload` re-POSTs the
config to the running process and never re-reads `EnvironmentFile=`. A Caddy process started before
`cloudflare.env` existed would stay blind to `CF_API_TOKEN` forever (`config.ts:59-67`). Tested at
`tests/app.test.ts:1875-1889`, which asserts the exact default string.

### 0.6 Orchestrator call parameters

| RPC | Deadline | Retry | On error | Source |
|---|---|---|---|---|
| ApplyConfig | 60 s | 1× only if the request was never sent (`grpcRequestNeverSent`) | throw | orch:`GrpcConfigPusher.ts:62,90-105,279-312` |
| ConfigureCaddy | 60 s | same | `{success:false}` | orch:`GrpcConfigPusher.ts:693-755` |
| GetSingBoxUserTraffic | **60 s** (default, not the observational 10 s) | same | `{success:false}` | orch:`GrpcConfigPusher.ts:350-364` |
| GetSingBoxConnections | 10 s | same | `{success:false}` | orch:`GrpcConfigPusher.ts:404-418` |

The contract guard (`assertContractAllowsMutation`) runs before all four calls. A `success!==false` reply
revives the telemetry stream and calls `healStaleUnreachable` (orch:`GrpcConfigPusher.ts:163-190`).

---

## 1. RPC `ApplyConfig`

Proto: `rpc ApplyConfig (ConfigPayload) returns (ConfigResponse)`. `ConfigPayload { string config_json = 1; bool force_reload = 2; }`,
`ConfigResponse { bool success = 1; string message = 2; }` (`proto/agent.proto:235-250`).
Handler: `applyConfigHandler` (`src/services/config.service.ts:99-220`), registered at `src/index.ts:148`.

### 1.1 Inputs actually read

- `configJson` (`|| config_json`, which is dead) (`config.service.ts:111`).
- `forceReload ?? force_reload`, coerced by `Boolean()` (`config.service.ts:193`). It is read **after**
  the skip check and validation, so it has no effect on the "unchanged" skip.

### 1.2 Who sends what

- Egress nodes: `SingBoxService.pushConfigToSingleNode` sends the generated egress config
  (orch:`src/core/SingBoxService.ts:210`). `forceReload=true` is set only for `user.revoked` and
  `user.disabled` webhooks (orch:`src/zones/remnawave-hooks/processors/userProcessor.ts:383-387`). Fleet
  pushes are debounced for 5 s (`userProcessor.ts:41`), and a pending `forceReload` is OR-ed across
  coalesced events (`:93`).
- Xeon fronts: `XeonService.pushConfigToNode` sends the ring config, always with `forceReload=false`
  (orch:`src/core/services/XeonService.ts:227`).
- The orchestrator calls ApplyConfig only when telemetry reports `singboxVersion !== 'not_installed'`
  (orch:`SingBoxService.ts:156-168`).
- Payload shape (generators): the egress config has the dispatcher `direct` inbound on `0.0.0.0:443`
  with `network:['udp']` (orch:`src/core/generators/egressSingbox.ts:486-497`). It has tuic and hysteria2
  inbounds on `127.0.0.1` with `users` (`[{name,uuid,password}]` / `[{name,password}]`, where `name` is
  the user uuid) (`egressSingbox.ts:172-228`). It always has
  `experimental.clash_api = {external_controller:"127.0.0.1:28080", secret: sha256hex(nodeSecret+"|clash-api/v1")}`
  (`egressSingbox.ts:542`, orch:`src/shared/utils/clashApi.ts:24,41-63`). The Xeon config has the
  same `clash_api` (orch:`src/core/generators/xeonSingbox.ts:263`) and a `direct` inbound `main-in` on
  `0.0.0.0`. The orchestrator **never** emits `services`. `users-api` is agent-injected.

### 1.3 Algorithm, in exact order (production, `NODE_ENV !== 'test'`)

1. **Auth.** The metadata secret only. On failure: `{success:false, message:'Invalid orchestrator secret token.'}` (`:103-108`).
2. **Parse.** `JSON.parse(rawConfig)` (`:112`). The shape is not validated. A parse error goes to the catch
   block and produces `Internal Agent Error: <V8 JSON error text>` (`:215-219`). An empty `config_json`
   gives `"Unexpected end of JSON input"`.
3. **Fork detection.** `isForkBinary()` (`src/utils/hotUsers.ts:66-69`) calls
   `getSingBoxVersion()` (`src/utils/telemetry.ts:129-144`), **uncached**, on every call:
   - `stat(SINGBOX_UNIT_FILE_PATH)`. If the unit is missing, the result is `'not_installed'`, so not a fork.
   - `execFile(SINGBOX_BINARY_PATH, ['version'])`. On any error, `'not_installed'`.
   - Parse (`telemetry.ts:124-127`): the first match of `/sing-box version (\S+)/`, else `/version\s+(\S+)/i`, else the first
     line trimmed, else `'not_installed'`. The whole token is kept, including any suffix.
   - `isFork = version.endsWith('-hotusers')` (`hotUsers.ts:38,68`).
4. **Inject users-api.** If `isFork`, `configObj = withUsersApiService(parsed)` (`hotUsers.ts:77-86`):
   - Let `services` be `config.services` if it is an array, else `[]`.
   - If any element has `type === 'users-api'`, return the input unchanged. This makes the step idempotent.
   - Otherwise return `{...config, services: [...services, {type:'users-api', tag:'users', path:'/run/sing-box/users.sock'}]}`.
     **Key order matters:** if `services` already exists, it keeps its position. If it does not, it is
     appended as the **last** key. The appended service has key order `type, tag, path`.
   - Injection happens **before** the on-disk comparison. Otherwise the comparison would never match
     (`config.service.ts:114-123`).
5. **Core running.** `isSingboxRunning()` (`singbox.ts:192-195`) calls `isSingboxUnitActive()`
   (`singbox.ts:171-183`), which runs `execAsync('systemctl is-active sing-box')`. The core counts as
   running only if stdout trimmed is `=== 'active'`. A rejection (any non-active state exits non-zero)
   means `false`.
6. **Image stale.** `isSingboxImageStale()` (`singbox.ts:216-233`):
   - `execAsync('systemctl show sing-box -p MainPID --value')`, then trim. If the result is empty or `'0'`, return `false`.
   - `fs.readlink('/proc/<pid>/exe')`. On error, `''`.
   - Return `target.endsWith(' (deleted)')`. Any exception means `false`. The fail-safe direction is
     deliberate: a false positive would restart the core on every push (`singbox.ts:228-231`).
7. **Unchanged skip** (`config.service.ts:162-169`): if `singboxConfigMatches(configObj) && coreRunning && !imageStale`, then:
   - `singboxConfigMatches` (`singbox.ts:244-247`) reads `SINGBOX_CONFIG_PATH` as UTF-8 and compares it with
     `serializeSingboxConfig(configObj)` = `JSON.stringify(obj, null, 2)` (`singbox.ts:239-241`). A missing
     file never matches.
   - The response is `{success:true, message:'Sing-box configuration already current — nothing to write or reload.'}` (the dash is U+2014).
   - **Not executed on this path:** validation, firewall sync, any write, any reload.
   - It must be `success:true`. `PurgeWorker` only purges a user after **every** node confirms
     (orch:`src/core/PurgeWorker.ts:57`, `src/types/index.ts:268-273`). A failure here would stall the
     queue forever (`config.service.ts:148-150`, `tests/singboxConfigUnchanged.test.ts:92-105`).
8. **Validate.** `validateSingBoxConfig(configObj)` (`singbox.ts:252-275`):
   - First it calls `fixCaddyPermissions()` (side effect, see §5.1). This happens on every validated ApplyConfig.
   - `targetDir = dirname(SINGBOX_CONFIG_PATH)`. It writes
     `<targetDir>/.config.check_<Date.now()>_<first 8 chars of randomUUID>.json` with `JSON.stringify(obj,null,2)`
     (`mkdir -p` first). Mode: process default (umask).
   - `execFile(SINGBOX_BINARY_PATH, ['check','-c',<checkfile>])`, with no timeout. The **on-disk** binary
     is used, which may differ from the running image.
   - The check file is always unlinked in `finally`, and unlink errors are ignored.
   - On failure, the result is `{valid:false, error: err.stderr || err.message}`. The handler replies
     `{success:false, message:'Rejected by Node Agent: Invalid sing-box syntax. Error: ' + error}` (`config.service.ts:173-178`).
   - In test mode, validation always returns `{valid:true}` and does not run `fixCaddyPermissions` (`singbox.ts:253-255`).
9. **Firewall sync.** `syncEgressFirewall(configObj)` (`src/utils/firewall.ts:140-196`). It never throws.
   - `isUfwInstalled()` runs `execAsync('command -v ufw')`. If that rejects, it warns and returns (`firewall.ts:23-30,141-144`).
   - `newPorts = extractUdpTunnelPorts(config)` (`firewall.ts:90-107`): for each inbound that is an object,
     **skip** if `type` is a string not in `{hysteria2, tuic}`. Also skip if `listen` (trimmed) ∈
     `{127.0.0.1, ::1, localhost}`. Then `Number(listen_port ?? port)` is accepted if it is an integer
     in 1..65535. The result is deduped in insertion order.
   - `previousPorts`: read `/opt/route-agent/active_ports.json` as a JSON array of numbers. Any error means `[]` (`firewall.ts:12,60-68`).
   - `planFirewallSync` (`firewall.ts:127-135`): `toOpen = new − prev`, `toClose = prev − new`,
     `needsReload = toOpen.length>0 || toClose.length>0`. If no reload is needed, it returns **without**
     rewriting the cache.
   - Otherwise it runs, in parallel, `sudo ufw allow <p>/udp` for each port to open (errors logged), then
     in parallel `sudo ufw delete allow <p>/udp` for each port to close (errors logged). Then it runs
     `sudo ufw reload` (errors logged), then writes the cache (`mkdir -p /opt/route-agent`,
     `JSON.stringify(ports,null,2)`, non-atomic).
   - In practice, current generators put tuic and hysteria2 on loopback and the dispatcher is `direct`.
     So the new set is empty, and any previously opened ports get closed once (`firewall.ts:85-88`).
10. **`forceReload`.** `Boolean(call.request.forceReload ?? call.request.force_reload)` (`config.service.ts:193`).
11. **Hot roster swap.** This runs only if `isFork && !forceReload && coreRunning && !imageStale`
    (`config.service.ts:203-208`). It calls `tryHotRosterSwap(configObj)` (`config.service.ts:57-94`):
    - `readSingboxConfigOnDisk()` (`singbox.ts:307-318`): read and `JSON.parse`. A missing file,
      unparsable JSON, or non-object yields `null`. On `null`, it warns
      `'Горячая замена невозможна: конфиг на диске не прочитался — уходим в перезагрузку'` and falls through to reload.
    - `plan = planRosterUpdate(configObj, onDisk)` (§3.2). On `null`, it warns with
      `{blocker: explainHotSwapBlocker(...)}` and the message `'Горячая замена невозможна — уходим в полную перезагрузку, сессии будут разорваны'`,
      then falls through.
    - `writeSingboxConfigAtomically(configObj)` (§1.4) **before** any socket call, then
      `applyRosterUpdates(plan)` (§3.3).
    - Success returns `{success:true, message:'Applied <plan.length> inbound roster change(s) through the live users-api — no reload, sessions kept.'}`.
      An empty plan is possible when the bytes differ but the parsed objects are equal, for example a
      whitespace-only difference on disk. In that case the reply is "Applied 0 …", and the file has been
      rewritten.
    - Any exception during write or PUT is logged as a warn (`'Hot roster swap failed — falling back to a full reload'`)
      and falls through to reload. Partial application is acceptable because the reload rebuilds from the
      file that has already been written (`config.service.ts:53-55`).
12. **Write and reload.** `atomicApplyAndReload(configObj)` (`singbox.ts:320-344`), see §1.4. Success
    returns `{success:true, message:'Configuration successfully validated, applied, and sing-box reloaded via gRPC channel.'}`.
    The text is the same whether the command was reload, start, or restart.
13. **Catch-all** (`config.service.ts:215-219`): `{success:false, message:'Internal Agent Error: ' + (err.message || 'Unknown error')}`.
    A failed reload lands here with the exec error message
    (`"Command failed: systemctl reload sing-box\n…"`).

### 1.4 File write and reload primitives

`writeSingboxConfigAtomically(obj)` (`singbox.ts:287-304`):

1. `targetDir = dirname(SINGBOX_CONFIG_PATH)`, with temp path `<targetDir>/.config.<Date.now()>_<8 chars of randomUUID>.tmp`.
2. If the config exists (`stat`), run `copyFile(config, config + '.bak')`. Errors are ignored.
3. `writeFile(tmp, JSON.stringify(obj,null,2), 'utf-8')`, with mode at the process default (typically 0644).
   There is **no** `mkdir -p` here. It relies on the directory existing, which validation created in the
   normal path. There is no fsync.
4. `rename(tmp, SINGBOX_CONFIG_PATH)`, which is atomic. The tmp file is not cleaned up if the write or
   rename throws.
5. It returns `{hadPreviousConfig}`.

`atomicApplyAndReload(obj)` (`singbox.ts:320-344`):

1. `writeSingboxConfigAtomically`.
2. If `NODE_ENV !== 'test' || process.env.RELOAD_COMMAND` is set, pick the command. In production this is
   `resolveSingboxReloadCommand()` (`singbox.ts:145-163`):
   - If the unit is not active (`systemctl is-active sing-box` ≠ `active`), use `'systemctl start sing-box'` (a literal, not configurable).
   - Else if `isSingboxImageStale()`, warn and use `config.SINGBOX_RESTART_COMMAND`.
   - Else use `config.RELOAD_COMMAND`.
   - In test mode it always uses `config.RELOAD_COMMAND`.
3. `execAsync(cmd)`. A non-empty stdout is logged at info and a non-empty stderr at warn.
4. On exec error: if there was a previous config, `copyFile(.bak, config)`, ignoring errors, then
   **rethrow**. **No second reload** is issued after the restore.

The unit's `ExecReload` is `sing-box check -c <cfg> && kill -HUP $MAINPID` (§5.2). sing-box's reload
is `instance.Close()` followed by a rebuild, so **every** reload drops all sessions
(`config.service.ts:131-135`, `hotUsers.ts:10-12`).

### 1.5 Response strings (exact)

| Case | success | message |
|---|---|---|
| bad secret | false | `Invalid orchestrator secret token.` |
| unchanged and core up and not stale | true | `Sing-box configuration already current — nothing to write or reload.` |
| `sing-box check` failed | false | `Rejected by Node Agent: Invalid sing-box syntax. Error: <stderr or err.message>` |
| hot swap OK | true | `Applied <N> inbound roster change(s) through the live users-api — no reload, sessions kept.` |
| reload path OK | true | `Configuration successfully validated, applied, and sing-box reloaded via gRPC channel.` |
| any thrown error | false | `Internal Agent Error: <err.message>` |

Orchestrator dependence: it uses only `success`, and shows `message` as a free-form error
(orch:`SingBoxService.ts:212-221`, `XeonService.ts:228-231`). The tests match `/already current/`
(`tests/singboxConfigUnchanged.test.ts:89,124,151,165`). No orchestrator code parses ApplyConfig messages.
Grep for `already current|Rejected by|Internal Agent Error` in orch `src/` found nothing.

### 1.6 Concurrency

There is no lock or in-flight dedupe. Two concurrent ApplyConfig calls race on `config.json.bak` (shared
name), on reloads, and on the hot-path read-compare-write sequence. Temp and check file names are unique.
The orchestrator's debounce (5 s) and per-node sequencing make collisions rare, but nothing prevents them.
For example, an egress fleet push can overlap a manual "Sync" press.

---

## 2. Unchanged detection, in detail (sing-box)

- Byte equality is between the on-disk file and `JSON.stringify(configObj, null, 2)`, where `configObj`
  is the parsed request, possibly with `users-api` injected. **Serialization is part of the contract.**
  The Go port must produce byte-identical output to V8's `JSON.stringify(x, null, 2)` for the same parsed
  input, or the first push after migration will not match. That costs one full reload per node, which is
  acceptable once. A *non-deterministic* serializer would make the skip never fire. Details:
  - **Object key order is preserved from the input JSON**, except that V8 orders "array-index-like" keys
    (canonical numeric strings `0`..`2^32-2`) first, ascending. Go `map[string]any` loses order. Use an
    ordered representation.
  - Indent is 2 spaces, the separator is `": "`, and there is no trailing newline. Empty objects print as
    `{}` and empty arrays as `[]`.
  - String escaping: only `"`, `\`, and U+0000–U+001F (`\b \f \n \r \t`, others as lowercase `\u00xx`), plus
    lone surrogates as `\udxxx`. `<`, `>`, `&`, U+2028, and U+2029 are **not** escaped. Go's
    `encoding/json` escapes them by default.
  - Numbers are IEEE-754 doubles printed in the JS shortest round-trip form (`1e+21`, `5e-7`). Integers above
    2^53 lose precision on parse. This is irrelevant for current configs, but byte equality depends on it.
- The skip requires all three of: bytes match, unit active, image not stale (`config.service.ts:144-163`).
  Tests: identical config, so the mtime is unchanged and the message matches `already current`
  (`tests/singboxConfigUnchanged.test.ts:73-90`). A changed config is written (`:107-125`). An inactive core
  (`SINGBOX_TEST_INACTIVE=1`) forces a write (`:127-151`). A missing file is never a match (`:153-166`).
- Test seams: `SINGBOX_TEST_INACTIVE=1` makes `isSingboxRunning` return false (`singbox.ts:193`), and
  `SINGBOX_TEST_STALE_IMAGE=1` makes `isSingboxImageStale` return true (`singbox.ts:219`). Both apply only
  under `NODE_ENV=test`.

---

## 3. Hot users (`src/utils/hotUsers.ts`)

### 3.1 Constants

- Socket `/run/sing-box/users.sock` (`hotUsers.ts:29`). The service object is
  `{type:'users-api', tag:'users', path:<socket>}` (`:31-35`).
- Fork suffix `-hotusers` (`:38`). Hot-swappable inbound types are `{vless, tuic, hysteria2}`
  (`:47`). The list is closed on purpose: other types would get a 404 *after* partial application
  (`:40-46`).

### 3.2 `planRosterUpdate(next, current)` (`hotUsers.ts:207-233`)

- If either side lacks an `inbounds` array, the result is `null`.
- `withoutUsers(cfg)` = `JSON.stringify({...cfg, inbounds: inbounds.map(i => {copy; delete users})})`, which is
  **compact** (no indent) and order-sensitive. If the two strings differ, the result is `null`. That covers
  any change outside `users`, added, removed, or reordered inbounds, key-order changes, and a newly appeared
  `services` key (`tests/hotUsers.test.ts:167-176`).
- For each index `i`: if `JSON.stringify(next.users ?? null) === JSON.stringify(current.users ?? null)`, the
  inbound is skipped. Otherwise the inbound needs `type` ∈ hot set, a non-empty string `tag`, and an array
  `users`. If any of these fails, the whole plan is `null`. Each passing inbound adds `{tag, users}`.
- It returns the update list in inbound order. `[]` means no difference.
- Since `withoutUsers` also strips `users` on `current`, "users changed from array to absent" is caught
  by the `Array.isArray` check and yields `null`.
- Tests: `tests/hotUsers.test.ts:59-176` (add, remove, both inbounds, identical → `[]`, port change, outbounds change,
  added inbound, shadowsocks, untagged, no inbounds, users-api appearing).

### 3.3 `putRoster` / `applyRosterUpdates` (`hotUsers.ts:242-281`)

- For each update, **sequentially**: an HTTP/1.1 request over the unix socket `/run/sing-box/users.sock`:
  `PUT /inbounds/<encodeURIComponent(tag)>/users`, with headers `content-type: application/json` and
  `content-length: <bytes>`. The body is `JSON.stringify(users)`, compact. This is the full roster, not a diff.
- Only status **200** counts as success. Anything else rejects with
  `users-api ответила <status> на инбаунд <tag>: <trimmed body>`. Transport errors reject with the socket error.
  **There is no timeout** (risk R5).
- Each success logs `{tag, users: count}`.
- The first failure aborts the remaining updates, which leaves a partial application. The caller then reloads (§1.3 step 11).

### 3.4 `explainHotSwapBlocker` / `describeConfigDiff` (`hotUsers.ts:124-198`)

These are for logs only and never return values. They never print values, which matters because configs
contain reality private keys and passwords (`hotUsers.ts:115-118`, tested at `tests/hotSwapBlocker.test.ts:72-87`).

- No `inbounds` in next gives `'в присланном конфиге нет inbounds'`. No `inbounds` on disk gives `'в конфиге на диске нет inbounds'`.
- A diff outside rosters gives `'разошлось вне наборов абонентов: ' + path`. Path format:
  - The root is `(корень)`, object keys join with `.`, and array indices use `[i]`.
  - An array length mismatch gives `<path>[длина <cur> → <next>]`.
  - Key-set differences give `<path>{добавлено: a|b, убрано: c}`, where `—` stands for an empty list.
    A pure order difference gives `<path>{порядок ключей}`. The key-order check runs before the value checks.
- Per-inbound reasons: `инбаунд <i> типа <type> не умеет горячую замену`, `у инбаунда <i> нет тега`, and
  `у инбаунда <i> набор абонентов не массив`. The fallback is `причина не определена`.
- A Go port should keep the same semantics. Exact strings are only logged, but parity keeps operator
  muscle memory.

---

## 4. RPC `ConfigureCaddy`

Proto: `CaddyConfigPayload { caddyfile_content=1; camouflage_path=2; camouflage_html=3; cloudflare_api_token=4; extra_cert_pem=5; extra_key_pem=6; extra_cert_domain=7; repeated CaddyExtraCertificate extra_certificates=8; }`,
`CaddyExtraCertificate { domain=1; cert_pem=2; key_pem=3; }`, `CaddyConfigResponse { success=1; message=2; }`
(`proto/agent.proto:498-552`). Handler: `configureCaddyHandler` (`config.service.ts:238-446`).

### 4.1 Who sends what

- Egress (`nodeOrchestrator.syncCaddyOnNode`, orch:`src/core/services/orchestrator/nodeOrchestrator.ts:1455-1507`):
  - The orchestrator-generated Caddyfile (`caddyfileGenerator.generate`), `camouflagePath` (default
    `/var/www/camouflage`), and `camouflageHtml` (orch `src/assets/camouflage/index.html`, or a fallback string;
    orch:`GrpcConfigPusher.ts:684-697`).
  - `extraCertificates` for **all** deliverable ring fronts, and optionally the single
    `extraCertPem`/`extraKeyPem`/`extraCertDomain` from the courier.
  - Optional fields are sent only when truthy (orch:`GrpcConfigPusher.ts:726-738`).
  - It **never** sends `cloudflareApiToken`.
- Xeon front (`XeonService.syncCaddyOnXeonNode`, orch:`src/core/services/XeonService.ts:326-350`): the Xeon
  Caddyfile, `camouflagePath:'/var/www/decoy'`, and `cloudflareApiToken: config.CLOUDFLARE_DNS_API_TOKEN`.

### 4.2 Algorithm (production)

1. **Auth** (`authenticateCall`). On failure: `{success:false, message:'Invalid orchestrator secret token.'}` (`:242-245`).
2. Inside `try`:
   - `finalCaddyfile = typeof caddyfileContent === 'string' ? caddyfileContent : ''` (`:268-269`). An empty
     string is **accepted and written** (risk R11).
   - The camouflage path is `camouflagePath || '/var/www/camouflage'`, then `sanitizeConfigInput`, which strips
     `\r` and `\n` and trims (`:27-30`). If that leaves nothing, the default is used. Then
     `validateSafeCamouflagePath` (`caddy.ts:128-145`) runs: `path.resolve` (no symlink resolution), and the result
     must be inside one of `/var/www`, `/tmp/camouflage`, `/opt/route-agent/decoy`, or `<process.cwd()>/tests/temp`
     (`caddy.ts:118-123`). Inside means `relative` does not start with `..` and is not absolute, and the root
     itself is allowed. Otherwise it throws `Path Traversal restriction: <input> is not within allowed directories`.
     The catch turns that into `{success:false, message:<that text>}`. Tested at `tests/app.test.ts:1854-1868`.
   - The HTML is `camouflageHtml || camouflage_html || htmlContent`. `htmlContent` is not in the proto, so it is dead.
3. **Build the planned-write list, in this exact order** (`:288-356`). `PlannedWrite = {path, content, mode?}` (`plannedWrites.ts:21-26`):
   1. If the HTML is a string and its trimmed length is > 0: `{<camouflagePath>/index.html, html}`. The content is
      **not** trimmed, and no mode is set.
   2. If `cloudflareApiToken` is a non-blank string: `{'/etc/caddy/cloudflare.env', 'CF_API_TOKEN=' + token + '\n', mode 0o600}`.
      The token is not trimmed and not sanitized (risk R12). The path is hard-coded and does not follow `CADDYFILE_PATH`.
   3. If `extraCertPem`, `extraKeyPem`, and `extraCertDomain` are all non-blank strings: compute
      `getCaddyCertPaths(domain)`, then add `{certPath, certPem}` and `{keyPath, keyPem, 0o600}`.
   4. For each `extraCertificates[]` entry: `domain` is trimmed. `cert = certPem` if it is a string, else
      `cert_pem`. Same for the key. Entries with an empty domain, a blank cert, or a blank key are **skipped
      silently**, because half a pair is worse than none (`:344-346`). Otherwise add the cert and the key (0o600).
      The raw, untrimmed domain is used for the path, but trimmed first.
   5. Always: `{CADDYFILE_PATH, finalCaddyfile}`, which is **last**.
4. **Unchanged skip** (`:365-371`): if `allWritesAlreadyOnDisk(plan) && isCaddyActive()`, reply
   `{success:true, message:'Caddy configuration already current — nothing to write or reload.'}`.
   - `allWritesAlreadyOnDisk` (`plannedWrites.ts:38-52`): an empty list gives `false`. For each write, the file
     content as UTF-8 must `===` the planned content. When `mode` is set, `stat.mode & 0o777` must equal it.
     **Modes are part of the comparison**, so a key whose permissions drifted forces a rewrite.
   - `isCaddyActive` (`config.service.ts:229-233`): `execAsync('systemctl is-active caddy')`, and stdout trimmed
     must be `=== 'active'`. Errors count as inactive. Test seam: `CADDY_TEST_INACTIVE=1` under `NODE_ENV=test`.
5. `mkdir -p dirname(CADDYFILE_PATH)` (`:373-374`).
6. **Backup:** if `CADDYFILE_PATH` exists, `copyFile → CADDYFILE_PATH + '.bak'`, ignoring errors (`:376-379`). Only
   the Caddyfile is backed up. Certs, HTML, and the env file have no backup or rollback.
7. **Writes:** `applyPlannedWrites(plan)` (`plannedWrites.ts:55-70`), in list order:
   - `mkdir -p dirname`. Then, with no mode: `writeFile(path, content, 'utf-8')`, which truncates in place.
     **This is not atomic.** New files get the umask default and existing files keep their mode.
   - With a mode: `writeFile(path, content, {mode})` (the mode applies only on create), then
     `chmod(path, mode)`, ignoring errors.
   - So the order is HTML, then CF env, then certs, then Caddyfile. Certs land before the Caddyfile because Caddy
     fails the **whole** config at provision time if any referenced cert file is missing (`:325-335`,
     incident 2026-09-03).
8. `fixCaddyPermissions()` (§5.1), which runs **unconditionally, in tests too**. It recursively applies `chown caddy:caddy` and
   `chmod 755` to `/var/lib/caddy`. **This changes the key files just written from 0600 to 0755** (risk R1).
9. `fixXraySocketPermissions()` (§5.4): `chmod 666` on `/dev/shm/vless-xhttp.sock` and `/dev/shm/vless-grpc.sock`, for each one that exists.
10. Only when `NODE_ENV !== 'test'`:
    1. `caddyBin = resolveCaddyBinary()` (`caddy.ts:24-32`): `fs.access(CADDY_BINARY_PATH || '/usr/local/bin/caddy-custom')`.
       If it exists, use that full path, else the literal `caddy` (found via `$PATH`). This is evaluated **per
       call** on disk, not from env at startup, so an upload mid-lifetime takes effect without restarting the agent
       (`caddy.ts:13-23`, tested at `tests/app.test.ts:622-646`).
    2. `cfEnv = loadCaddyDnsProviderEnv()` (`caddy.ts:44-55`): read `/etc/caddy/cloudflare.env`. A missing file
       gives `{}`. Each line is trimmed, and empty lines and lines starting with `#` are skipped. The line is split at
       the first `=`. Neither the key nor the value is trimmed further, so `A = b` gives key `A ` and value ` b`. Tested at
       `tests/app.test.ts:648-662`.
    3. `execAsync('<caddyBin> validate --config "<CADDYFILE_PATH>"', {env: {...process.env, ...cfEnv}})`
       (`config.service.ts:400`). There is no `--adapter` flag: Caddy auto-selects the caddyfile adapter only because
       the filename starts with `Caddyfile`. The env merge exists because `EnvironmentFile=` reaches only
       systemd-spawned processes (`config.service.ts:394-397`).
       - On failure: restore `.bak` over the Caddyfile if it existed before (errors ignored). Then
         `valMsg = (err.stderr || err.stdout || err.message || 'Ошибка синтаксиса Caddyfile').trim()`, and the reply is
         `{success:false, message:'Ошибка синтаксиса Caddyfile:\n' + valMsg}` (`:401-411`).
    4. `execAsync(config.CADDY_RELOAD_COMMAND)` with the inherited env (`:415`).
       - On failure: restore `.bak` (if one existed before). Then `execAsync('journalctl -u caddy -n 12 --no-pager')`,
         keeping trimmed stdout and ignoring errors. The reply is
         `{success:false, message:'Сбой службы Caddy при reload:\n' + (journalLogs || err.message)}` (`:416-434`).
         **No re-reload or restart happens after the restore** (risk R7).
11. Success: `{success:true, message:'Caddyfile успешно валидирован и применен.'}` (`:437-440`). Tested at
    `tests/app.test.ts:911-938` (exact message, HTML written, Caddyfile written).
12. Catch-all: `{success:false, message: err.message}` (`:441-445`). This covers write failures, path traversal, and so on.

### 4.3 Orchestrator dependence on the response text

- `nodeOrchestrator.syncCaddyOnNode` first sends a Caddyfile with `proxy_protocol` listener wrappers. If that
  fails **and** `/proxy_protocol/.test(message)`, it retries without them, treating the node as Caddy < 2.7
  (orch:`nodeOrchestrator.ts:1482-1506,1538`). **The raw `caddy validate` stderr/stdout must therefore be
  included in the failure message**, for example
  `Ошибка синтаксиса Caddyfile:\nmodule not registered: caddy.listeners.proxy_protocol`
  (orch:`tests/egressCaddyRealIp.test.ts:119`). Paraphrasing or truncating it breaks the fallback.
- On failure the orchestrator assumes **the agent rolled the Caddyfile back**, so the previous ring-proxy mark stays
  valid (orch:`nodeOrchestrator.ts:1487-1488`).
- The fixtures in orch tests use the prefixes `Ошибка синтаксиса Caddyfile` and `Сбой службы Caddy при reload`
  (orch:`tests/ringProxyProtocolEgress.test.ts:145`, `tests/fleetDriftSweep.test.ts:106`). Keep those prefixes byte-exact.

### 4.4 Unchanged-detection tests

`tests/caddyConfigUnchanged.test.ts` covers five cases:

- Identical: mtime unchanged and `/already current/` (`:68-83`).
- Changed Caddyfile is written (`:86-97`).
- Changed cert with identical Caddyfile is **not** a skip (`:99-117`).
- Caddy down forces a rewrite (`:119-136`).
- Drifted key mode is rewritten and must end up `0600` (`:138-169`). **This last test fails when run as root.**
  It was run in this session: `493 !== 384`, meaning 0755 after the handler, because of step 8. In non-root CI
  it passes vacuously, since `/var/lib/caddy` cannot be created and the test returns early at `:161`. See risk R1.

### 4.5 Cert path layout (`src/utils/certStorage.ts`)

- `sanitizeDomainForStorage` (`certStorage.ts:9-17`) applies, in order: `' '→'_'`, `'+'→'_plus_'`,
  `'*'→'wildcard_'`, `':'→'-'`, removes every `..`, then lower-cases. It mirrors certmagic `KeyBuilder.Safe()`.
- Base: `/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory` (`:19-20`).
- Paths: `<base>/<safe>/<safe>.crt` and `<base>/<safe>/<safe>.key` (`:22-29`).
- This must be identical to orch:`src/shared/utils/certStorage.ts`, which uses `path.posix`. The orchestrator
  embeds these exact paths in the Caddyfile and in sing-box `certificate_path`/`key_path`.

### 4.6 Concurrency

There is no lock. Concurrent ConfigureCaddy calls share `Caddyfile.bak` and write files non-atomically.
The orchestrator has produced two identical calls within one second (bot step plus push pre-sync,
`plannedWrites.ts:7-11`). The unchanged-skip makes the second one cheap only if the first finished.

---

## 5. Helpers in `singbox.ts` / `caddy.ts`

### 5.1 `fixCaddyPermissions(dir='/var/lib/caddy', runExec=execAsync)` (`singbox.ts:32-45`)

If `stat(dir)` succeeds, it runs `chown -R caddy:caddy <dir> || true` and then `chmod -R 755 <dir> || true`, in
that order, through a shell. Errors are logged at warn and never thrown. The owner is hard-coded because the apt
package always creates `caddy:caddy` (`singbox.ts:13-28`). Tested at `tests/app.test.ts:1911-1940`: chown is
first, both include the dir, and nothing runs if the dir is missing.

It is called from two places: ConfigureCaddy step 8 (always), and `validateSingBoxConfig`, which is also used by the
rear core (`src/utils/rearCore.ts:103`, `src/services/rearSingbox.service.ts:9`).

### 5.2 `ensureSingboxSystemdUnit(overridePath?, runExec)` (`singbox.ts:61-134`)

This is called by the sing-box binary upload (`src/services/binary.service.ts:206`), not by the RPCs in this group.

- Path: `overridePath || SINGBOX_UNIT_FILE_PATH`. Exact content (`singbox.ts:93-105`), with the binary and config paths interpolated:
  ```
  [Unit]
  Description=sing-box service
  After=network.target nss-lookup.target

  [Service]
  ExecStart=<bin> run -c <cfg>
  Restart=always
  RestartSec=5
  ExecReload=/bin/sh -c "<bin> check -c <cfg> && /bin/kill -HUP $MAINPID"

  [Install]
  WantedBy=multi-user.target
  ```
  The file ends with a newline. There is deliberately **no** `CapabilityBoundingSet`/`AmbientCapabilities`. Those lines
  removed `CAP_KILL` from `ExecReload`, so reload never worked on a node where the process ran as another user
  (incident mo-nl-node, 2026-09-04; `singbox.ts:69-92`). Tested at `tests/app.test.ts:575-620`.
- If the existing content is byte-equal, it returns `false` and makes no exec calls. Otherwise it runs `mkdir -p`,
  `writeFile` (non-atomic), and (non-test only) `systemctl daemon-reload` (errors logged), then returns `true`.
  Any other error is logged and returns `false`.

### 5.3 `ensureCaddyCustomBinaryOverride(overridePath, runExec)` (`caddy.ts:78-116`)

This is called by `UploadCaddyBinary` (`binary.service.ts:721`).

- Path `/etc/systemd/system/caddy.service.d/override.conf` (`caddy.ts:9`). The binary is
  `CADDY_BINARY_PATH || /usr/local/bin/caddy-custom` (`:11,82`). Content (`caddy.ts:91-97`):
  ```
  [Service]
  ExecStart=
  ExecStart=<bin> run --environ --config <CADDYFILE_PATH>
  ExecReload=
  ExecReload=<bin> reload --config <CADDYFILE_PATH> --force
  EnvironmentFile=-/etc/caddy/cloudflare.env
  ```
  The empty `ExecStart=` and `ExecReload=` lines reset the apt unit's values. Without the reset, `reload` would run
  the stock binary, which cannot parse `dns cloudflare` (`caddy.ts:85-90`).
- If the content is equal, it returns `false`. Otherwise it writes and, in non-test mode, runs `systemctl daemon-reload`
  and then `systemctl restart caddy`. **Errors propagate** (it does not catch). It returns `true`. A restart is required
  because `daemon-reload` alone leaves the old process running (`caddy.ts:66-73`). Tested at `tests/app.test.ts:664-692`.

### 5.4 `fixXraySocketPermissions(paths, runExec)` (`caddy.ts:180-196`)

The default paths are `/dev/shm/vless-xhttp.sock` and `/dev/shm/vless-grpc.sock` (`caddy.ts:153-156`). For each
path that exists (`stat`), it runs `chmod 666 <path>`. Errors are logged per path and processing continues. It never
throws. It is also called **once at agent startup** (`src/index.ts:122`). Tested at `tests/app.test.ts:1942-1989`.

### 5.5 `resolveSingboxReloadCommand`, `isSingboxUnitActive`, `isSingboxImageStale`

See §1.3 and §1.4. Tests: inactive → `systemctl start sing-box`, including when `is-active` *rejects*; active →
`RELOAD_COMMAND` (`tests/app.test.ts:694-720`); stale → `SINGBOX_RESTART_COMMAND` (`:722-753`).

---

## 6. RPC `GetSingBoxUserTraffic`

Proto: `SingBoxUserTrafficRequest { orchestrator_secret = 1 }`,
`SingBoxUserTrafficEntry { string user_uuid=1; int64 uplink_bytes=2; int64 downlink_bytes=3; }`,
`SingBoxUserTrafficResponse { bool success=1; string message=2; repeated entries=3; }` (`proto/agent.proto:622-636`).
Handler: `src/services/singboxStats.service.ts:14-50`.

### 6.1 Algorithm

1. Auth via `authenticateCall`. On failure: `{success:false, message:'Invalid orchestrator secret token.', entries:[]}` (`:18-21`).
2. `queryUserTrafficDeltas()` (`src/utils/singboxStats.ts:118-129`):
   - `endpoint = readClashApiEndpoint()` (§7.2). If it is `null`, return `null`, **without touching state**.
   - `records = fetchConnections(endpoint)` (§7.3). A throw propagates, and state is untouched.
   - `{deltas, next} = computeUserTrafficDeltas(records, lastTotals, !primed)`. Then `lastTotals = next` and `primed = true`.
3. If the result is `null`: `{success:true, message:'clash_api block absent from the applied sing-box config', entries:[]}` (`:25-34`).
   The success is intentional. The specific message makes a fleet-wide regression visible (`:26-33`).
4. Otherwise: `{success:true, message:'OK', entries: deltas.map(d => ({userUuid, uplinkBytes, downlinkBytes}))}` (`:36-44`).
5. Catch: `{success:false, message: err.message, entries:[]}` (`:45-49`), with a warn log.

### 6.2 `computeUserTrafficDeltas(records, previous, isFirstPass)` (`singboxStats.ts:59-103`). A pure function.

For each record, in order:

- Skip it if `id === ''` or `user === ''`. It is neither counted nor remembered (`:71`). Xeon fronts have an empty
  `user`, so fronts always yield no entries.
- If `next.size < 100_000` (`MAX_TRACKED_CONNECTIONS`, `:40`), set `next[id] = {upload, download}` (`:77-79`).
- On the first pass, skip counting (`:81`), so the first call after agent start returns `[]` (`tests/singboxStats.test.ts:40-52`).
- `prev = previous[id]` or `{0,0}`. Then `up = max(0, upload − prev.upload)` and `down = max(0, download − prev.download)`.
  If both are 0, skip (`:83-93`).
- Accumulate per `user` in a Map. **Output order is the first-seen order of users.**
- Ids not seen in this pass are dropped from state (`tests/singboxStats.test.ts:85-91`).

Tests (`tests/singboxStats.test.ts`): only increments (`:54-59`), a new connection counts in full (`:61-71`), a
closed connection seen twice is not double-counted (`:73-83`), per-user attribution (`:93-108`), a counter moving
backwards gives 0 (`:110-117`), and empty user or id is dropped (`:119-130`).

### 6.3 Global state

`lastTotals: Map<string,{upload,download}>` and `primed: boolean` (`singboxStats.ts:42,49`) are module-level and
process-lifetime. They reset only on restart (`resetTrafficDeltaState` is tests only, `:106-109`). They are **not**
shared with `GetSingBoxConnections`, which is stateless. There is no lock (risk R9).

### 6.4 Orchestrator consumption

`connectionsSweepService` treats `!success` as "no data" at debug level, and for each entry computes
`delta = uplinkBytes + downlinkBytes` (orch:`src/zones/anomaly-traffic/services/connectionsSweepService.ts:550-565`).
See risk R-O1: under `longs: String` these are strings.

---

## 7. RPC `GetSingBoxConnections`

Proto: `SingBoxConnectionsRequest { orchestrator_secret=1 }`. `SingBoxConnection` has `user=1`, `source_ip=2`,
`destination_ip=3`, `destination_domain=4`, `uint32 destination_port=5`, `int64 upload_bytes=6`, `int64 download_bytes=7`,
`network=8`, `inbound_tag=9`, `int64 started_at_unix_ms=10`, and `bool closed=11`. The response is
`{success=1, message=2, repeated connections=3}` (`proto/agent.proto:638-671`).
Handler: `src/services/singboxConnections.service.ts:21-46`. The rear variant, `GetRearConnections` (`:58-79`), is
in the same file but out of scope.

### 7.1 Algorithm

1. Auth via `authenticateCall`. On failure: `{success:false, message:'Invalid orchestrator secret token.', connections:[]}`.
2. `endpoint = readClashApiEndpoint()`. If it is null: `{success:true, message:'clash_api not configured on this node', connections:[]}`.
3. `connections = fetchConnections(endpoint)`. The reply is `{success:true, message:'OK', connections}`.
   The internal `id` is dropped by the serializer.
4. Catch: `{success:false, message: err.message, connections:[]}`, with a warn log.

### 7.2 `readClashApiEndpoint(configPath=SINGBOX_CONFIG_PATH)` (`src/utils/singboxConnections.ts:58-76`)

It reads the **applied** config file and parses it as JSON. Then `addr = parsed.experimental.clash_api.external_controller`.
If that is not a string or is blank after trimming, the result is `null`. Otherwise it returns
`{address: addr.trim(), secret: typeof secret==='string' ? secret : ''}`. Any read or parse error gives `null`
(debug log). Tested at `tests/singboxConnections.test.ts:110-146`.

### 7.3 `fetchConnections(endpoint)` (`singboxConnections.ts:166-183`)

- Exactly **one** request: `GET http://<address>/connections`. There is no `?closed=true`, because that endpoint
  does not exist. The query was ignored and every connection was counted twice until 2026-09-15
  (`singboxConnections.ts:148-164`, tested at `tests/singboxConnections.test.ts:160-179`).
- The header `authorization: Bearer <secret>` is sent only if the secret is non-empty. The timeout is 5000 ms
  (`AbortSignal.timeout`).
- A non-2xx response throws `clash api responded <status> for /connections`. A JSON parse error throws.
- The result goes to `parseClashConnections(json, closed=false)`, so **`closed` is always false** today.

### 7.4 `parseClashConnections(payload, closed)` (`singboxConnections.ts:122-145`)

- If `payload.connections` is not an array, the result is `[]`. This includes `null` and `{}` (`tests/singboxConnections.test.ts:103-107`).
- For each raw entry, with `meta = raw.metadata ?? {}`:
  | Output | Source | Coercion |
  |---|---|---|
  | `id` | `raw.id` | `asString`: a string or `''` |
  | `user` | `meta.user` | `asString` |
  | `sourceIp` | `meta.sourceIP` | `asString` |
  | `destinationIp` | `meta.destinationIP` | `asString` |
  | `destinationDomain` | `meta.host` | `asString` |
  | `destinationPort` | `meta.destinationPort` | `asNumber`: a finite number is kept. A string goes through JS `Number()` (`''`→0, `' 443 '`→443, `'0x1BB'`→443, `'1e3'`→1000), and non-finite gives 0. Anything else gives 0. |
  | `uploadBytes` / `downloadBytes` | `raw.upload` / `raw.download` | `asNumber` |
  | `network` | `meta.network` | `asString` |
  | `inboundTag` | `meta.inboundTag` | `asString` |
  | `startedAtUnixMs` | `raw.start` | a non-empty string goes through `Date.parse` (RFC3339 with ns precision truncates to ms; it was verified that `Date.parse('…T10:00:00.123456789+03:00')` gives ms). NaN or a non-string gives 0. |
  | `closed` | the argument | |
- A bad field zeroes only that field (`tests/singboxConnections.test.ts:81-95`).
- On serialization to the wire, protobufjs coerces `destinationPort` to uint32 and the bytes and time fields to int64.

---

## 8. `src/utils/listenerWarning.ts` (used by `PullNodeWarnings`)

This is invoked by `pullNodeWarningsHandler` in parallel with the journal and rear-provider checks. The result is
put **first** in the warning list (`src/services/nodeWarnings.service.ts:44-52`).

- `CORE_NOT_LISTENING_KIND = 'core_not_listening'` (`listenerWarning.ts:28`). The orchestrator dedups on it.
- `publicListenPorts(cfg)` (`:36-55`): for each inbound object, `listen` is the string value or `''`. Skip the
  inbound if `listen` is non-empty and not `0.0.0.0` or `::`. **`127.0.0.1` and any specific IP count as
  non-public.** If `listen_port` is a number and an integer > 0, collect it, deduped. `network` and `type` are ignored.
- `parseListeningPorts(ssOut)` (`:58-68`): for each line, match `/[\s%][^\s]*:(\d+)\s+[^\s]+:\*/` and collect the
  integer port, deduped. This takes the local port, not the peer port (`tests/listenerWarning.test.ts:62-64`).
- `detectCoreNotListening(runExec)` (`:78-115`):
  1. Read `SINGBOX_CONFIG_PATH || '/etc/sing-box/config.json'`. If the file is missing or empty, the result is `null`.
     If JSON is invalid, `null`.
  2. Compute the expected ports. If there are none, return `null` **without running `ss`**
     (`tests/listenerWarning.test.ts:84-102`).
  3. Run `ss -lnu` and `ss -lnt` in parallel through the shell, with no timeout. If either throws, return `null`
     (debug log, `tests/listenerWarning.test.ts:115-126`).
  4. `listening` is the union of the UDP and TCP ports, with no protocol distinction (risk R2).
     `missing` is the expected ports not in `listening`. If none are missing, return `null`.
  5. Otherwise return `{kind:'core_not_listening', source:'sing-box', sample:'конфиг объявляет порты <expected joined ", ">, но никто не слушает <missing joined ", ">', occurrences: missing.length, lastSeenUnixMs: Date.now()}`.
- It does **not** check whether the unit is active, despite the doc comment (`:71-77`).

---

## 9. Other handlers in `config.service.ts` (brief)

- **`GetManagedCertificate`** (`config.service.ts:455-487`): auth via `authenticateCall` (`CertificateRequest`
  has `orchestrator_secret`). If `domain` is missing, not a string, or blank, the reply is
  `{success:false, message:'Missing domain parameter.'}`. Otherwise it reads
  `getCaddyCertPaths(domain)` `.crt` and `.key` in parallel (the domain is **not trimmed** here, unlike in
  ConfigureCaddy) and replies `{success:true, message:'OK', fullchainPem, privateKeyPem}`. On a read error:
  `{success:false, message:<fs error>}`, with a warn log.
- **`ConfigureAwg`** (`config.service.ts:497-730`) and `awgBoolDirective` (`:38-43`) are expected to be in the AWG
  group's spec. Key facts so nothing is lost: auth is metadata only. `enabled=false` runs
  `systemctl stop route-awg@<if> || systemctl stop awg-quick@<if> || awg-quick down <if> || true`, and with ufw it
  deletes the port. Otherwise it writes `AWG_CONFIG_PATH` non-atomically and with no mode. It reloads the unit,
  falls back to restart, or starts it if the interface is missing. Then `systemctl enable`, iptables
  FORWARD/MASQUERADE `-C || -A`, optional ip6tables trap, and ufw allow+reload. The success message is
  `AmneziaWG (AWG3) interface awg0 configured and active on port <port||51820>.` The interface name in the message
  is hard-coded `awg0`.

---

## 10. Background timers and loops in scope

- **None** of the modules in this group starts a timer.
- At startup, `fixXraySocketPermissions()` runs once (`src/index.ts:122`).
- The process-lifetime state that matters is `lastTotals`/`primed` in `singboxStats.ts:42,49`. `getSingBoxVersionCached`
  (60 s cache, `telemetry.ts:146-162`) is **not** used by `isForkBinary`, which calls the uncached version.

---

## 11. Parity checklist

Each item is a concrete, testable behavior.

**Auth and transport**
1. Every handler in scope answers gRPC status OK with `success:false` and `message === 'Invalid orchestrator secret token.'` on a bad or empty secret. Traffic and Connections also return empty repeated fields.
2. ApplyConfig accepts the secret only from the `x-orchestrator-secret` metadata (first value). ConfigureCaddy, Traffic, and Connections also accept the request field `orchestrator_secret` where the proto has one.
3. The secret comparison is constant-time, and an empty `EGRESS_CONTROL_SECRET` rejects everything.

**ApplyConfig**
4. Invalid JSON gives `success:false` with the prefix `Internal Agent Error: `.
5. With a stock binary (version not ending in `-hotusers`) the config is written without a `services` entry. With a fork binary, `{"type":"users-api","tag":"users","path":"/run/sing-box/users.sock"}` is appended to `services` (the key is created last if absent), and it is never duplicated.
6. If the bytes on disk equal the 2-space-indented serialization (V8-compatible key order and escaping) **and** `systemctl is-active sing-box` prints `active` **and** `/proc/<MainPID>/exe` does not end with ` (deleted)`, the agent runs nothing else (no check, no ufw, no write, no reload). It replies `success:true` with the exact `already current` message, and the file mtime is unchanged.
7. The same config with the unit inactive leads to validate, write, and `systemctl start sing-box`.
8. The same config with a stale image leads to validate, write, and `SINGBOX_RESTART_COMMAND`. The hot path is skipped.
9. `sing-box check -c <dir>/.config.check_*.json` runs on a temp file in the config's directory. The temp file is always removed. On failure, the reply is `Rejected by Node Agent: Invalid sing-box syntax. Error: <stderr or error text>`, and nothing is written.
10. The ufw sync opens and closes only non-loopback tuic/hysteria2 UDP ports. It runs `sudo ufw reload` and rewrites `/opt/route-agent/active_ports.json` only when the set changed. If ufw is absent, it is a no-op.
11. With a fork binary, `force_reload=false`, the core active, and the image fresh, a roster-only change writes the file, sends `PUT /inbounds/<urlencoded tag>/users` with the full JSON array over `/run/sing-box/users.sock` for each changed inbound in order, and replies `Applied N inbound roster change(s) …`. No reload.
12. Any non-roster diff, an unknown inbound type, an empty tag, non-array users, an unreadable disk config, or a non-200 or failed PUT falls back to the full reload path.
13. `force_reload=true` never takes the hot path.
14. Writes are atomic via `.config.<ts>_<rand>.tmp` plus rename in the same dir, and the previous file is copied to `config.json.bak` first.
15. If the reload command fails, `.bak` is restored (if a previous config existed) and the reply is `Internal Agent Error: <exec error>`. No second reload happens.
16. Reload command choice: inactive gives `systemctl start sing-box`, stale gives `SINGBOX_RESTART_COMMAND`, otherwise `RELOAD_COMMAND`.
17. The success text is identical for reload, start, and restart.

**ConfigureCaddy**
18. The default camouflage path is `/var/www/camouflage`. CR/LF are stripped and the value is trimmed. Paths outside the 4 allowed roots fail with `Path Traversal restriction: … is not within allowed directories`.
19. HTML is written to `<path>/index.html` only when it is non-blank, and the content is written verbatim.
20. A Cloudflare token is written to `/etc/caddy/cloudflare.env` as `CF_API_TOKEN=<token>\n` with mode 0600, only when non-blank.
21. Cert pairs (single fields and every complete bulk entry) are written to the certmagic-safe paths, with the key at 0600, **before** the Caddyfile. Incomplete bulk entries are skipped silently.
22. If every planned file already has identical content (and an identical mode where one is specified) **and** `systemctl is-active caddy` prints `active`, nothing is written, validated, or reloaded. The reply is `success:true` with `Caddy configuration already current — nothing to write or reload.`
23. A changed cert alone breaks the skip. Caddy being inactive breaks the skip. A drifted key mode breaks the skip.
24. The Caddyfile is backed up to `<path>.bak` before writes, only if it exists.
25. Validation is `<caddyBin> validate --config "<path>"` with `/etc/caddy/cloudflare.env` merged into the env. `caddyBin` is `CADDY_BINARY_PATH` or `/usr/local/bin/caddy-custom` when that file exists **at call time**, else `caddy`.
26. On a validation failure, `.bak` is restored and the reply starts with `Ошибка синтаксиса Caddyfile:\n` and **contains Caddy's raw output**. The orchestrator greps for `proxy_protocol`.
27. On a reload failure (the command is `systemctl reload caddy || systemctl restart caddy` by default), `.bak` is restored, the reply is `Сбой службы Caddy при reload:\n<last 12 journal lines of caddy, or the error text>`.
28. The success text is exactly `Caddyfile успешно валидирован и применен.`
29. After the writes: `chown -R caddy:caddy` then `chmod -R 755` on `/var/lib/caddy` (if it exists), and `chmod 666` on whichever of the two xray sockets exist. Reproduce this, or consciously fix R1 and document the decision.

**Traffic and Connections**
30. If the applied config has no `experimental.clash_api.external_controller`, Traffic replies `success:true, message:'clash_api block absent from the applied sing-box config', entries:[]` and Connections replies `success:true, message:'clash_api not configured on this node', connections:[]`.
31. Exactly one HTTP GET goes to `http://<external_controller>/connections`, with `Authorization: Bearer <secret>` only when the secret is non-empty, and a 5 s timeout. A non-2xx response gives `success:false, message:'clash api responded <code> for /connections'`.
32. The field mapping and coercions are as in the §7.4 table. `closed` is always false, and `id` is not on the wire.
33. The first Traffic call after process start returns `[]` and primes state. Later calls return per-user increments, never negative values, for records with a non-empty id and user. Records absent from a pass are forgotten, and at most 100 000 are tracked.
34. A Traffic call that hits "no clash_api" or an HTTP error does not mutate the delta state.

**Listener warning**
35. With no config, no public inbounds, or no `ss`, the result is `null`. With public ports (`listen` empty, `0.0.0.0`, or `::`) that are not in the union of `ss -lnu` and `ss -lnt` local ports, the result is a warning with the exact `sample` format and `occurrences = missing count`.

**Unit files**
36. The sing-box unit and the Caddy override contents are byte-exact as in §5.2 and §5.3. Writes are idempotent, with daemon-reload only on change, and the Caddy override also restarts caddy on change.

---

## 12. Risks and smells

These are flagged, not fixed. Decide each one consciously in the Go port.

- **R1: `fixCaddyPermissions` makes private keys world-readable and defeats the Caddy unchanged-skip.**
  ConfigureCaddy writes keys at 0600 (`config.service.ts:322,350`, `plannedWrites.ts:64-68`), then runs
  `chmod -R 755 /var/lib/caddy` (`config.service.ts:383`, `singbox.ts:40`). ApplyConfig's validation runs it again
  (`singbox.ts:256`). Consequences:
  1. Every ring/wildcard private key on every egress node ends up `0755`.
  2. `allWritesAlreadyOnDisk` demands 0600 for keys, so **on any node that receives certs the "already current"
     skip can never fire**. Each ConfigureCaddy validates and reloads Caddy, which is the exact cost the skip was
     built to avoid.
  3. The test `tests/caddyConfigUnchanged.test.ts:138-169` fails as root (`493 !== 384`, run during this analysis)
     and passes vacuously in non-root CI.
  A faithful port reproduces the waste. A fixed port changes observable behavior: there would be fewer Caddy
  reloads. In any fix, sing-box (root) and Caddy (owner) still need read access to the keys.
- **R2: The listener check ignores protocol.** `ss -lnu` and `ss -lnt` ports are merged (`listenerWarning.ts:98-99`), and
  `network` is ignored (`:36-55`). On egress, the sing-box dispatcher declares `0.0.0.0:443` with
  `network:['udp']`, while Caddy holds TCP 443. If sing-box stops listening on UDP 443, which is the motivating
  incident, Caddy's TCP 443 hides it and no warning is raised. Xeon `main-in` (TCP and UDP) has the same issue.
- **R3: The Caddy rollback is partial.** Only the Caddyfile has a `.bak`. Certs, `index.html`, and `cloudflare.env`
  stay at their new values after a validation or reload failure (`config.service.ts:376-434`). Writes are
  in-place truncation, not atomic, so a crash mid-write leaves a truncated Caddyfile or key.
- **R4: The sing-box `.bak` is clobbered after a hot-path failure.** `tryHotRosterSwap` writes the new config, which
  first copies the old file to `.bak`. If the PUT then fails, `atomicApplyAndReload` writes again and copies the
  **new** file to `.bak`. If that reload fails, the "restore" restores the new config, so the rollback target is
  lost (`config.service.ts:86-92`, `singbox.ts:294-297,338-340`).
- **R5: No timeouts on exec or the users-api socket.** `execAsync`/`execFileAsync` have no timeout (`exec.ts`). That
  covers `sing-box version`, `sing-box check`, `systemctl …`, `caddy validate`, `ss`, `ufw`, and `journalctl`.
  `putRoster` has no request timeout (`hotUsers.ts:245-266`). A hung fork socket or a stuck `systemctl` blocks the
  handler indefinitely. The orchestrator gives up after 60 s, and the agent keeps going.
- **R6: No serialization of concurrent applies.** There is no mutex in ApplyConfig or ConfigureCaddy. Shared `.bak`
  names, interleaved reloads, and hot-path read-compare-write races are all possible (§1.6, §4.6).
- **R7: After a failed reload, nothing restarts the service.** For sing-box, `.bak` is restored but not reloaded. If
  `systemctl start`/`restart` failed, the core stays down until the next push. For Caddy, if `reload` failed and the
  `|| restart` fallback also failed, Caddy is down with the restored file and is not restarted.
- **R8: Config files are world-readable.** The sing-box config (reality keys, passwords, clash secret) is created by
  `writeFile` at the default mode, typically 0644 (`singbox.ts:300`). So is the check file. The AWG config
  (private key) is written the same way (`config.service.ts:640`).
- **R9: Traffic delta state is unsynchronized and capped.** Two concurrent `GetSingBoxUserTraffic` calls both read the
  old `lastTotals` and double-report (`singboxStats.ts:118-129`). Beyond 100 000 tracked connections, untracked ones
  are reported in full on every pass (`singboxStats.ts:77-79`). Connections that open and close between polls, and the
  final increment of a connection that closes between polls, are never reported (`singboxConnections.ts:157-160`). The
  proto comment claiming closed-buffer coverage is stale (`proto/agent.proto:642-644,663-664`), and `closed` is always false.
- **R10: Clash API field names are unverified (undetermined).** The parser expects `metadata.user` and
  `metadata.inboundTag` (`singboxConnections.ts:79-93`). Upstream sing-box's Clash tracker JSON, as far as recalled,
  emits `type` (`"<inboundType>/<inboundTag>"`) and **no** `user` key. If that is true for the deployed stock/fork
  builds, `user` is always `''`, and `GetSingBoxUserTraffic` silently returns `[]` everywhere (records with an empty user
  are dropped). That is the same "looks healthy, reports nothing" failure mode the module comments warn about. Verify
  against a live node's `/connections` before the port.
- **R11: An empty Caddyfile is accepted.** A missing `caddyfile_content` writes an empty Caddyfile, then validates and
  reloads it (`config.service.ts:268-269,356`).
- **R12: No sanitization for the env file and the Caddy validate path.** The Cloudflare token goes into
  `cloudflare.env` verbatim, so an embedded newline injects extra env lines (`config.service.ts:298-305`).
  `CADDYFILE_PATH` is interpolated into a shell string inside double quotes (`config.service.ts:400`). It comes from the
  agent's own env, so this is low risk.
- **R13: The camouflage path allow-list.** It includes `<cwd>/tests/temp` in production (`caddy.ts:122`), allows writing
  directly into the root (`/var/www/index.html`), and does not resolve symlinks (`caddy.ts:128-145`).
- **R14: A stale socket list.** `fixXraySocketPermissions` handles only `vless-xhttp.sock` and `vless-grpc.sock`
  (`caddy.ts:153-156`). The orchestrator's Caddyfile also proxies to `/dev/shm/vless-xhttp-cdn.sock` (whitelist/CDN
  inbound, orch:`src/core/generators/caddy.ts:95,190`), and that socket is never chmodded. The comment says the list
  must be kept in sync by hand (`caddy.ts:147-152`).
- **R15: The Caddy validate adapter is implicit.** `caddy validate --config <path>` relies on the filename starting with
  `Caddyfile` to select the caddyfile adapter. A custom `CADDYFILE_PATH` with another name would be parsed as JSON.
- **R16: Extra work per ApplyConfig.** `isForkBinary` runs `sing-box version` on every push, uncached
  (`hotUsers.ts:66-69`). `isSingboxImageStale` runs twice on the reload path (`config.service.ts:162`,
  `singbox.ts:157`).
- **R17: Firewall extraction edge case.** An inbound whose `type` is missing or not a string is **not** skipped
  (`firewall.ts:96`), so its `listen_port` is opened in ufw. Port opens and closes run in parallel `sudo ufw`
  processes, and ufw is not designed for concurrent writers (`firewall.ts:158-180`).
- **R18: Check file inside the config dir.** `validateSingBoxConfig` drops `.config.check_*.json` into `/etc/sing-box`
  (`singbox.ts:257-259`). A package-style unit using `-C /etc/sing-box` merges every `*.json` in that directory, which
  is the hazard documented for the rear config at `config.ts:29-34`.
- **R-O1 (orchestrator-side, wire-neutral): int64 fields arrive as strings.** With `longs: String`,
  orch:`connectionsSweepService.ts:561` computes `entry.uplinkBytes + entry.downlinkBytes` on **strings**, which is
  concatenation (`"500"+"600"="500600"`), so the burst and accumulator math is wrong. `takeTop` sorting uses
  subtraction, which coerces correctly. This does not change what the agent must send (int64 on the wire), but
  watch for it when testing end to end.
- **R-O2: Dead code paths.** The snake_case fallbacks and `htmlContent` are dead (§0.1). The `authenticateCall` payload
  fallback is dead for ConfigureCaddy. The Go port can drop them without behavior change.

---

## 13. Undetermined

- Whether the deployed sing-box Clash API (stock 1.12/1.13 and the `-hotusers` fork) emits `metadata.user` and
  `metadata.inboundTag` (R10). It could not be verified without the sing-box source or a live node.
- The exact users-api response semantics beyond 200/400/404 (fork README not in repo).
- Whether a Caddy reload failure that falls through to a failed restart leaves Caddy stopped in practice (R7). This
  depends on systemd unit behavior on the nodes.
