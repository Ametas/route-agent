# Group C — binaries, scripts, rule sets, kernel-module source

Spec of the TypeScript agent as of `go-rewrite` @ `a6b1055`. Source of truth for the Go port together
with the TS tests. Paths are relative to `/home/user/route-agent` unless prefixed `orch:` (=
`/home/user/vpn-orchestrate`).

RPCs covered (all declared in `proto/agent.proto:16-97`):

| RPC | Kind | Handler | Uses shared receiver? |
|---|---|---|---|
| `UpgradeSingbox` | unary | `src/services/binary.service.ts:302` `upgradeSingboxHandler` | n/a |
| `UploadSingboxBinary` | client-stream | `src/services/binary.service.ts:81` | no (own copy) |
| `UploadAwgToolsBinary` | client-stream | `src/services/binary.service.ts:390` | no (own copy) |
| `UploadAwgGoBinary` | client-stream | `src/services/binary.service.ts:537` | no (own copy) |
| `UploadCaddyBinary` | client-stream | `src/services/binary.service.ts:645` | no (own copy) |
| `UploadMihomoBinary` | client-stream | `src/services/mihomo.service.ts:58` | **yes** `receiveStreamedBinary` |
| `UploadRearRuleSet` | client-stream | `src/services/mihomo.service.ts:137` | **yes** |
| `UploadOlcrtcAgentSrvBinary` | client-stream | `src/services/olcrtc.service.ts:363` | no (own copy) |
| `UploadMeshAwgKernelSource` | client-stream | `src/services/meshTunnel.service.ts:270` | no (own copy) |

Registration: `src/index.ts:151-164`. Startup hooks in scope: `sanitizeAwgToolsSymlink()` and
`ensureOlcrtcAgentSrvSystemdUnit()` (`src/index.ts:119,121`).

The orchestrator is the only client. Every client-stream RPC is driven by one helper,
`orch:src/core/services/grpc/GrpcBinaryUploader.ts`; `UpgradeSingbox` by
`orch:src/core/services/grpc/GrpcConfigPusher.ts:797-819` — and **nothing in the orchestrator calls
`UpgradeSingbox` any more** (only definition `orch:src/core/services/grpcAgent.ts:194-198`; the bot
"📦 Update" button runs node-op `singbox_upgrade`, which uses `UploadSingboxBinary`,
`orch:src/core/services/nodeOps/actions.ts:553-562`).

---

## 0. Cross-cutting mechanics (apply to every RPC below unless stated otherwise)

### 0.1 Wire decoding

- Proto loaded with `keepCase:false, longs:String, enums:String, defaults:true, oneofs:true`
  (`src/index.ts:51-57`). So the handler sees camelCase: `orchestratorSecret`, `chunk` (Buffer),
  `version`, `isFinal`, `targetBinary`. The code also probes snake_case
  (`data?.orchestrator_secret`, `data.target_binary`) — these fallbacks are dead with
  `keepCase:false` but harmless.
- **`is_final` is never read by any handler.** Completion = the client half-closing the stream
  (`call.on('end')`). The proto comment on `UploadMeshAwgKernelSource` ("On the final chunk the
  agent extracts…", `proto/agent.proto:95`) is inaccurate: work starts at stream end.
- `version`: per chunk, `if (data.version) version = data.version` — last **non-empty** value wins;
  default `'unknown'` (`binaryReceiver.ts:67,93`; `binary.service.ts:91,114-116` and copies).
  Mesh handler never reads `version` at all.
- `targetBinary`: same "last non-empty wins", default `''` (`binaryReceiver.ts:94`,
  `binary.service.ts:424-426`). Only AwgTools, RearRuleSet read it. Others ignore it.
- `chunk`: appended only when `data.chunk && data.chunk.length > 0`. Empty chunks are ignored
  (no file is created for them).
- No server-side `grpc.max_receive_message_length` override (`src/index.ts:126-145`) → grpc-js
  default 4 MiB per message. Orchestrator sends 64 KiB chunks
  (`orch:GrpcBinaryUploader.ts:153`). Go's default server max-recv is also 4 MiB — keep it.

### 0.2 Authentication (stream RPCs)

Secret = `config.EGRESS_CONTROL_SECRET` (`src/config.ts:9`, required, no default).
`verifySecret` (`src/middleware/auth.ts:12-23`): false for empty/non-string token, false if
expected secret is empty, length mismatch → false, else `crypto.timingSafeEqual` on UTF-8 bytes.
Metadata key `x-orchestrator-secret`, first value only (`auth.ts:28-31`).

Per-stream algorithm (identical in all 8 stream handlers; receiver version `binaryReceiver.ts:61-102`):

1. `secretVerified = verifySecret(metadataSecret)` at call start.
2. On **each** `data` event while not aborted: if not yet verified and the chunk's
   `orchestratorSecret` verifies → `secretVerified = true` (sticky; later chunks are not re-checked;
   a wrong payload secret after a good metadata secret is ignored).
3. If still not verified after step 2 → `isAborted = true`, log warn `Unauthorized <RPC> attempt
   rejected`, **`callback(null, {success:false, message:'Invalid orchestrator secret token.'})`
   (gRPC status OK)**, then `call.destroy(new Error('PermissionDenied: Invalid orchestrator secret
   token.'))`. Subsequent `data`/`end` are ignored (`isAborted`).
   - Consequence: the secret must be valid in metadata **or in the first chunk**. A stream whose
     first chunk lacks a payload secret (and metadata is bad) is rejected even if later chunks carry it.
   - Tests rely on status OK + `success:false`: `tests/app.test.ts:493-508` (olcrtc, `assert.ifError(err)`),
     `tests/app.test.ts:1140-1151` (mesh), `tests/binaryReceiver.test.ts:118-131` (asserts
     `destroyed` is set). `tests/app.test.ts:429-456` (sing-box) accepts either an error or the response.
4. On `end` with **zero `data` events** and bad metadata (only reachable path to the end-time check):
   - Receiver / Singbox / Olcrtc: unlink temp, warn, same callback `'Invalid orchestrator secret
     token.'` + destroy (`binaryReceiver.ts:111-120`, `binary.service.ts:134-143`, `olcrtc.service.ts:416-425`).
   - AwgTools / AwgGo / Caddy: combined check `!secretVerified || bytesWritten === 0` →
     `'No valid binary data received or unauthorized.'`, **no destroy** (`binary.service.ts:447-450,590-593,698-701`).
   - Mesh: `'No valid source data received or unauthorized.'`, no destroy (`meshTunnel.service.ts:325-328`).

`UpgradeSingbox` (unary) checks metadata only — `UpgradePayload` has no secret field
(`binary.service.ts:306-310`).

### 0.3 Receiving to a temp file

- Temp path built at call start: `` `/tmp/<prefix>_${Date.now()}_${randomUUID().slice(0,8)}.tmp` ``
  — hard-coded `/tmp`, not `os.tmpdir()` (except Mesh, see §9). Prefixes:

  | RPC | temp path |
  |---|---|
  | Singbox | `/tmp/sing-box_<ms>_<8hex>.tmp` (`binary.service.ts:90`) |
  | UpgradeSingbox | `/tmp/sing-box_<ms>_<8hex>.tmp` (`binary.service.ts:331`) |
  | AwgTools | `/tmp/awg_tools_<ms>_<8hex>.tmp` (`:399`) |
  | AwgGo | `/tmp/amneziawg_go_<ms>_<8hex>.tmp` (`:546`) |
  | Caddy | `/tmp/caddy_custom_<ms>_<8hex>.tmp` (`:654`) |
  | Mihomo | `/tmp/mihomo_<ms>_<8hex>.tmp` (`mihomo.service.ts:65` + `binaryReceiver.ts:66`) |
  | RearRuleSet | `/tmp/ruleset_<ms>_<8hex>.tmp` (`mihomo.service.ts:144`) |
  | Olcrtc | `/tmp/olcrtc-agent-srv_<ms>_<8hex>.tmp` (`olcrtc.service.ts:372`) |
  | Mesh | `path.join(os.tmpdir(), 'amneziawg_kernel_src_<ms>_<8hex>.tar.gz')` (`meshTunnel.service.ts:285`) |

- File created lazily on the first non-empty chunk via `fs.createWriteStream(tempPath)` (default
  flags `'w'`, mode `0o666 & ~umask`; agent runs as root under systemd with default umask →
  `0644`, owner root:root; `install.sh:284-291`). Writes are **not awaited** and backpressure is
  ignored (`fileStream.write(buf)` return value discarded) — Node buffers in memory if disk is slow.
- `bytesWritten` counts bytes handed to `write()`.
- On `end`: `await fileStream.end(resolve)` (flush; no `fsync`). Then auth / empty checks (§0.2).
  `bytesWritten === 0` → unlink temp, `{success:false, message:'No binary data received.'}`
  (Receiver/Singbox/Olcrtc) or the combined messages above.
- **No size limit, no checksum, no integrity verification of any kind.** The only content checks
  are per-RPC smoke tests (sing-box `version`, mihomo `-v`, awg-quick shebang + `bash -n`).
- No `error` listener on the WriteStream: an fs write error (ENOSPC/EIO) emits an unhandled
  `'error'` → **uncaught exception, agent process crashes** (systemd `Restart=always` restarts it).
  See Risks.
- `call.on('error')` / `call.on('cancelled')`: `fileStream.end()` (if any) and fire-and-forget
  unlink of the temp path (`binaryReceiver.ts:72-75,143-147`; copies e.g. `binary.service.ts:260-271`).
  **These do NOT stop an `end` handler that is already running** — an apply in progress continues
  (swap, restarts, DKMS build) even if the client cancels/disconnects; only the temp file may vanish
  under it (then a later `copyFile`/`chmod` fails with ENOENT → failure response that nobody reads).
- Temp file removal: all handlers unlink the temp on every exit path (explicit unlinks + catch);
  the receiver unlinks **before** sending the response (`binaryReceiver.ts:127-140`, comment: the
  "done" answer must mean the temp is gone). Copies unlink right after the swap, also before callback.

### 0.4 `replaceBinaryAtomically(sourcePath, targetPath)` (`binary.service.ts:56-76`)

1. `mkdir -p dirname(target)` (mode default 0777&~umask = 0755).
2. `tempTarget = <dir>/.<basename(target)>_<Date.now()>_<randomUUID().slice(0,6)>.tmp`.
3. `fs.copyFile(source, tempTarget)` (content copy; no xattrs → capabilities set on the source are lost).
4. `fs.chmod(tempTarget, 0o755)`.
5. `fs.rename(tempTarget, target)` — atomic within same dir; replaces a running executable without
   ETXTBSY; if `target` is a symlink, the symlink itself is replaced.
6. `finally`: unlink `tempTarget` (ignored errors; no-op after successful rename).
- No `fsync` of file or directory. No backup of the previous binary → **no rollback exists
  anywhere in group C**; the safety model is "validate the candidate in /tmp before swapping".
- Owner stays root:root (process is root). Test: `tests/app.test.ts:1891-1909` (creates missing dir,
  overwrites existing).

### 0.5 External command semantics (must be reproduced, including error text)

- `execAsync` = `promisify(child_process.exec)` → `/bin/sh -c "<string>"`; `execFileAsync` =
  `promisify(execFile)` → direct spawn (`src/utils/exec.ts`). Default `maxBuffer` 1 MiB per stream
  (exceeding kills the child, error message `stdout maxBuffer length exceeded`); **no timeout unless
  passed** (only the Mesh path passes timeouts). Timeout → SIGTERM to the direct child, error
  `killed:true`, message `Command failed: <cmd>\n<stderr>`.
- Rejection message format (verified on Node 22): `` `Command failed: ${cmd}\n${stderr}` `` where for
  `execFile` `cmd = [file, ...args].join(' ')`. stdout is NOT in the message. Spawn failure:
  `spawn <file> ENOENT` / `EACCES`. This text flows into response messages (`Failed to upload
  binary: …`, `Failed to install kernel module: …`) and the orchestrator parses it:
  `orch:src/shared/utils/failureDigest.ts:6-9,39-55` takes line 1 as "what was run" and picks cause
  lines (`:\d+:\d+: error:`, `^Error! `, `^dpkg: error processing package `, `^E: `) from the rest.
  **Go must keep `Command failed: <cmd>\n<stderr>` shape** for messages that embed command errors.
- `execFile` of a file that is not a valid executable image: Node/libuv falls back to running it
  via `/bin/sh` (ENOEXEC behaviour), observed: `Command failed: ./x version\n./x: 1: …: not found`
  (exit 127). Go `exec.Command` returns `exec format error` instead. Message differs; outcome (failure) same.

### 0.6 Test seam

`process.env.NODE_ENV === 'test'` skips: smoke tests (`version`, `-v`), `setcap`, live-config checks,
all `systemctl` calls, `restartActiveAwgServices`, Caddy override, DKMS toolchain, `modprobe`.
The Go port needs an equivalent seam (or injectable runner) for the parity tests listed in §11.

### 0.7 Concurrency / global state

- **Agent side: no locks at all.** Concurrent uploads of the same binary each get their own temp and
  `tempTarget` names; both swap (last `rename` wins), both restart the service. Concurrent rule sets
  with the same name in the same millisecond share a staging path (§7) — one may fail with ENOENT.
- Agent global state touched: in-memory version caches (`invalidateSingboxVersionCache`,
  `invalidateAwgVersionCache`, `invalidateMeshAwgKernelStatusCache`, `src/utils/telemetry.ts:159,335,391`).
- Orchestrator side dedupe (`orch:GrpcBinaryUploader.ts:21-31,51-66`): key
  `${address}:${grpcPort}:${targetBinary}`; same version in flight → join the running promise;
  different version → queued after it. Per orchestrator process only.

### 0.8 Orchestrator client behaviour (what the agent must accept)

`orch:GrpcBinaryUploader.ts:39-181`:
- Metadata `x-orchestrator-secret: <secret>` (`orch:GrpcConnectionManager.ts:329-333`) **and** every
  chunk carries `{orchestratorSecret, chunk, version, isFinal, targetBinary}`.
- 64 KiB chunks; `isFinal = offset+64KiB >= len`. Drain-aware, yields every 16 chunks. `call.end()` after the loop.
  An empty buffer would send **zero chunks** then end → agent answers with the "no data" message.
- **No deadline** on upload calls (`invokeUpload` passes no call options).
- `targetBinary` values sent: `'sing-box'`, `'awg'` (default) / `'awg-quick'`, `'amneziawg-go'`,
  `'caddy-custom'`, `'olcrtc-agent-srv'`, `'mihomo'`, rule-set name (version `''`),
  `'amneziawg-kernel-src'` (`:183-349`).
- Response handling: gRPC error → `{success:false, message:'gRPC error: <err.message>'}`; otherwise
  the agent response is returned **verbatim**; `response.success !== false` triggers
  `grpcTelemetryStreamManager.checkAndReviveStream(node)` (`:91-101`).
- Consumers only branch on `success`; `message` is shown in node journal / bot / system channel
  (`orch:nodeOps/actions.ts:292-310`, `orch:RearSingBoxService.ts:104-131`,
  `orch:meshTunnelOrchestrator.ts:378-385,442-449`, `orch:XeonService.ts:375-405`). No exact-string
  matching of agent upload messages was found in the orchestrator (grep of every message below).
  The only structural dependency is the `Command failed:` multi-line shape (§0.5).

---

## 1. `UploadSingboxBinary` (`binary.service.ts:81-272`)

Inputs read: metadata secret; per chunk `orchestratorSecret`, `version`, `chunk`. (`targetBinary` ignored.)

Side effects on `end`, in order (prod = `NODE_ENV!=='test'`, Linux):

1. Wait for flush; auth check; `bytesWritten===0` → `'No binary data received.'` (§0.2/0.3).
2. `targetPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box'` (default from schema
   `src/config.ts:11`).
3. `mkdir -p /tmp` (no-op), `chmod <temp> 0755`.
4. prod: `execFile(<temp>, ['version'])`, no timeout. Rejection → outer catch (step E).
5. prod & linux: `exec("setcap 'cap_net_admin,cap_net_bind_service=+ep' <temp>")` — failure only
   logged (`Failed to setcap on new sing-box binary`). (Effectively useless: caps don't survive copyFile, see §0.4.)
6. prod: `verifyBinaryAcceptsLiveConfigs(<temp>)` (`binary.service.ts:40-54`):
   - configs `[SINGBOX_CONFIG_PATH ('/etc/sing-box/config.json'), REAR_SINGBOX_CONFIG_PATH
     ('/etc/route-agent/rear.json')].filter(Boolean)`, checked **in that order**, first failure wins;
   - missing file (`fs.stat` fails) → skipped;
   - `execFile(<temp>, ['check','-c',<configPath>])`, no timeout; on rejection error text =
     `(stderr || message || 'unknown error').slice(0,1000)`.
   - The mihomo config (`/etc/route-agent/rear.yaml`) is intentionally NOT in this list
     (`src/config.ts:38-41`).
   - Reject → unlink temp, log error, respond
     `` `Бинарь ${targetVersion} отвергнут: он не принимает уже применённый на узле конфиг ${configPath}. Прежний бинарь оставлен на месте, сервис не тронут.\n${error}` `` (`:178-183`), `success:false`.
   - Note: `install.sh:141` seeds `/etc/sing-box/config.json` with `{"route":{"rules":[]}}`, so on a
     fresh node the front check runs against that stub.
7. `replaceBinaryAtomically(<temp>, targetPath)`; unlink temp.
8. prod & linux: `exec("setcap 'cap_net_admin,cap_net_bind_service=+ep' <targetPath>")`, failure logged only.
9. log `Atomically updated sing-box binary`; `invalidateSingboxVersionCache()` (runs in test too).
10. `unitChanged = await ensureSingboxSystemdUnit()` (runs in test too; `src/utils/singbox.ts:61-134`):
    - unit path `SINGBOX_UNIT_FILE_PATH` (`/etc/systemd/system/sing-box.service`), content exactly:
      ```
      [Unit]
      Description=sing-box service
      After=network.target nss-lookup.target

      [Service]
      ExecStart=<BIN> run -c <CFG>
      Restart=always
      RestartSec=5
      ExecReload=/bin/sh -c "<BIN> check -c <CFG> && /bin/kill -HUP $MAINPID"

      [Install]
      WantedBy=multi-user.target
      ```
      (trailing newline; `$MAINPID` literal). Byte-compare with existing file; equal → return false.
    - Differs/missing → `mkdir -p`, `writeFile` (0644, non-atomic), prod: `systemctl daemon-reload`
      (failure logged), return true. Any fs error → logged, return **false**.
    - Must NOT contain `CapabilityBoundingSet`/`AmbientCapabilities` (`singbox.ts:69-92`,
      `tests/app.test.ts:593-606`): bounding set stripped CAP_KILL from `ExecReload` → reload broke
      on a node whose process belonged to user `sing-box` (mo-nl-node, 2026-09-04).
11. prod: if `unitChanged` → `exec('systemctl enable sing-box')` (no `--now`; failure logged).
12. prod: **unconditional** `exec(config.SINGBOX_RESTART_COMMAND)` (default
    `'systemctl restart sing-box'`, `src/config.ts:25`); stdout logged info, stderr logged warn,
    rejection logged warn — **never changes the response**.
13. Respond `{success:true, message:`sing-box binary version ${targetVersion} successfully updated and restarted`}`
    (even if restart failed).

E. Any throw in steps 2-12 (e.g. `version` smoke test, chmod, copy/rename): log, unlink temp,
`` `Failed to upload binary: ${msg}` `` (`:252-257`).

Why (quotes):
- restart, not reload/enable --now: "Только `restart` делает exec нового файла, и он же поднимает
  юнит, если тот был неактивен" (`binary.service.ts:209-228`); incident 2026-09-20: fork on disk,
  old process running, agent injected `users-api` → `unknown inbound type: users-api`.
  Enforced structurally by `tests/singboxBinarySwap.test.ts:36-72` (restart call present, after
  `if (unitChanged)`, not in an `else`, no `execAsync(config.RELOAD_COMMAND` / `systemctl enable --now`).
- live-config check before swap: "Проверяем ДО подмены, во временном файле: так откат не нужен
  вовсе" (`:168-169`); fork rejects bad UUIDs where upstream silently `uuid.NewV5`s (`:21-39`);
  tests `tests/singboxBinaryCompat.test.ts` (no configs → ok; rejecting front → names `config.json`;
  rejecting rear → names `rear.json`).
- Unit provisioned lazily on upload, not by install.sh (`:201-205`, `install.sh:135-138`).

Post-swap observability (other groups, but parity-relevant):
- Telemetry `singbox_version` = `getSingBoxVersion()` (`src/utils/telemetry.ts:129-145`): unit file
  missing → `'not_installed'`; else `execFile(BIN,['version'])` parsed by
  `parseSingBoxVersionOutput` (`:124-127`, regex `/sing-box version (\S+)/` then `/version\s+(\S+)/i`,
  else first line, else `'not_installed'`). 60 s cache (`:147-162`) — invalidated in step 9 so the next
  2 s telemetry tick shows the new version. Fork = suffix `-hotusers` (`src/utils/hotUsers.ts:38,66-69`).
  Tests: `tests/singboxInstalledSignal.test.ts`.
- First `ApplyConfig` after the swap reads fork-ness from disk (`config.service.ts:124`) and uses
  `isSingboxImageStale()` (`/proc/<MainPID>/exe` ends with ` (deleted)`, `singbox.ts:216-235`) to
  restart instead of reload if step 12 did not take. `tests/hotSwapBlocker.test.ts` covers the
  hot-roster explanation in that path (group A), not this handler.

## 2. `UpgradeSingbox` (`binary.service.ts:302-385`) — unary, unused by the orchestrator

Inputs: metadata secret; `version`, `downloadUrl` (`download_url` fallback dead).

1. Bad secret → `'Invalid orchestrator secret token.'` (test `tests/app.test.ts:876-886`).
2. `url = downloadUrl`; `targetVersion = version || 'latest'`.
3. Empty url → `'Missing download_url in request payload.'` (test `tests/app.test.ts:888-898`).
4. `new URL(url)` throws → `'Invalid download_url format.'`; protocol not `http:`/`https:` →
   `'Invalid URL protocol: must be http or https.'`.
5. `execFile('curl', ['-fsSL','--connect-timeout','10','--max-time','300','--proto','=http,https','--', url, '-o', tempPath])`.
6. `chmod temp 0755`; prod: `execFile(temp,['version'])`, `setcap` on temp (warn on failure).
7. `replaceBinaryAtomically`; unlink temp; prod&linux `setcap` on target (warn).
8. `invalidateSingboxVersionCache()`.
9. prod: `exec(config.RELOAD_COMMAND || 'systemctl restart sing-box')` — **default
   `RELOAD_COMMAND` is `'systemctl reload sing-box'`** (`src/config.ts:13`), so the `||` fallback is
   dead and this path *reloads*. Failure logged only.
10. `` `sing-box binary version ${targetVersion} successfully upgraded from ${url}` ``.
- Catch: unlink temp, `` `Failed to upgrade sing-box binary: ${msg}` ``.
- No live-config check, no `ensureSingboxSystemdUnit`, no restart.

**Actual production behaviour (verified with curl 8.5.0):** after `--`, curl treats `-o` and the
temp path as additional URLs. The download goes to stdout, then `Could not resolve host: -o` →
non-zero exit → step 5 rejects (or, for a >1 MiB body, `stdout maxBuffer length exceeded`). The temp
file is never created. Moreover the orchestrator's URL is a `.tar.gz`
(`orch:GrpcConfigPusher.ts:801-802`), which could never pass `version` anyway. ⇒ In prod this RPC
**always** returns `success:false` with `Failed to upgrade sing-box binary: Command failed: curl … \n<curl stderr>`
(or the maxBuffer text) and changes nothing on disk. Client deadline would be 10 min
(`orch:GrpcConfigPusher.ts:87`). Owner decision needed on what Go should do (see §13).

## 3. `UploadAwgToolsBinary` (`binary.service.ts:390-532`)

Inputs: secret, `version`, `targetBinary`, `chunk`.

1. Flush; `!secretVerified || bytesWritten===0` → unlink, `'No valid binary data received or unauthorized.'`.
2. `isAwgQuick = targetBinary === 'awg-quick'` (exact). Anything else (`'awg'`, `'wg'`, `''`,
   garbage) → tools path.
   `targetPath = isAwgQuick ? (AWG_QUICK_BINARY_PATH || '/usr/local/bin/awg-quick') : (AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg')`.
3. awg-quick only:
   - Read the **whole** temp as UTF-8 (read error → `''`); must `startsWith('#!')` else unlink,
     warn, `'Invalid awg-quick script: missing shebang (#!) header.'` (test `tests/app.test.ts:838-857`).
   - If `process.platform !== 'win32'` (runs in tests too): `exec("bash -n <temp>")`, no timeout;
     failure → `errMsg = (stderr || stdout || message).trim()`, unlink,
     `` `Invalid awg-quick script syntax: ${errMsg}` `` (stderr contains the temp path).
4. `chmod temp 0755`; `replaceBinaryAtomically`; unlink temp.
5. `invalidateAwgVersionCache()` (clears tools+go caches).
6. prod: `restartActiveAwgServices()` (`src/utils/awg.ts:38-91`):
   - `exec("systemctl list-units 'route-awg@*' 'awg-quick@*' --state=active --plain --no-legend")`;
     each trimmed line matched `^(route-awg@\S+|awg-quick@\S+)` (unit name incl. `.service`); errors ignored.
   - none found → `exec('systemctl is-active route-awg@<iface>')` where iface =
     `basename(AWG_CONFIG_PATH,'.conf')` (`awg0`); stdout `active` → add.
   - none → `{reloaded:false}`. Else `exec('systemctl restart <unit>')` sequentially; failures logged;
     `reloaded = restarted.length > 0`.
   - NB: this also restarts the **mesh** interface `route-awg@awgmesh0` if active.
   - `serviceMsg = reloaded ? ' and active AWG service(s) restarted' : ' (service restart skipped: not yet configured)'`; test env → `''`.
7. Respond `` `${label} version ${targetVersion} successfully updated${serviceMsg}` `` with label
   `'awg-quick script'` | `'awg-tools (awg) binary'` (tests `tests/app.test.ts:795-836`).
- Catch: unlink, `` `Failed to upload binary: ${msg}` ``.
- No smoke test of the `awg` binary.

Startup self-heal `sanitizeAwgToolsSymlink()` (`binary.service.ts:282-297`, called `src/index.ts:119`):
`lstat(AWG_TOOLS_BINARY_PATH||'/usr/local/bin/awg')`; if symlink → `readlink` (for log), warn
`Self-heal: removed legacy symlink at AWG_TOOLS_BINARY_PATH created by deprecated UploadAwgBinary RPC`,
`unlink`. All errors swallowed. Test `tests/app.test.ts:510-535`.

Orchestrator usage: tools then awg-quick as two separate RPC calls with the same version
(`orch:awgOrchestrator.ts:146-155`, `orch:meshTunnelOrchestrator.ts:220-225`, `orch:nodeOps/actions.ts:344-355`).

## 4. `UploadAwgGoBinary` (`binary.service.ts:537-638`)

Same receive/auth as §3 (combined message). Then: `targetPath = AWG_GO_BINARY_PATH || '/usr/local/bin/amneziawg-go'`;
`chmod temp 0755`; `replaceBinaryAtomically`; unlink; `invalidateAwgVersionCache()`; prod:
`restartActiveAwgServices()` (same serviceMsg rule). Respond
`` `amneziawg-go binary version ${targetVersion} successfully updated${serviceMsg}` `` (test
`tests/app.test.ts:859-874`). No smoke test. Catch → `Failed to upload binary: …`.

## 5. `UploadCaddyBinary` (`binary.service.ts:645-748`)

Same receive/auth as §3. Then:
1. `targetPath = config.CADDY_BINARY_PATH || '/usr/local/bin/caddy-custom'` (`CADDY_BINARY_PATH` optional, no default, `src/config.ts:69-72`).
2. `chmod temp 0755`; `replaceBinaryAtomically`; unlink. **No smoke test, no Caddyfile validation with the new binary.**
3. prod: `restarted = await ensureCaddyCustomBinaryOverride()` (`src/utils/caddy.ts:78-116`):
   - path `/etc/systemd/system/caddy.service.d/override.conf`, content exactly:
     ```
     [Service]
     ExecStart=
     ExecStart=<BIN> run --environ --config <CADDYFILE>
     ExecReload=
     ExecReload=<BIN> reload --config <CADDYFILE> --force
     EnvironmentFile=-/etc/caddy/cloudflare.env
     ```
     `<BIN>` = `CADDY_BINARY_PATH || '/usr/local/bin/caddy-custom'`, `<CADDYFILE>` = `CADDYFILE_PATH || '/etc/caddy/Caddyfile'`.
   - Equal → return false. Else `mkdir -p`, `writeFile`, prod: `systemctl daemon-reload` then
     `systemctl restart caddy` — **not** wrapped: a failure throws out of the handler → `Failed to upload binary: Command failed: …` **after the binary was already swapped**. Return true.
   - Test `tests/app.test.ts:664-692`.
4. prod: `if (!restarted) await exec('systemctl restart caddy')` — also unguarded (throws → failure
   response, binary already swapped). `overrideMsg = ' and caddy.service restarted on it'` (prod) / `''` (test).
5. Respond `` `Custom Caddy binary version ${targetVersion} successfully updated${overrideMsg}` ``.

Why: restart even when drop-in unchanged — "работающий процесс держит старый бинарь в памяти"
(`binary.service.ts:713-718`); restart not reload for Type=notify (`caddy.ts:66-74`); ExecReload must
be overridden too or reload keeps invoking apt `caddy` ("module not registered", `caddy.ts:85-90`).
Other groups pick the custom binary by **on-disk presence at call time**
(`resolveCaddyBinary`, `caddy.ts:12-31`, test `tests/app.test.ts:629-648`) — the upload must make it
visible without an agent restart. Callers: Xeon seeding (`orch:XeonService.ts:398-405`), front op
`custom caddy` (`orch:nodeOps/frontActions.ts:293`), and since 2026-09-27 egress nodes with apt
Caddy < 2.7 (`binary.service.ts:640-643`).

## 6. `UploadMihomoBinary` (`mihomo.service.ts:58-120`, via receiver)

Receiver (§0) then apply(`{tempPath, version}`):
1. `chmod temp 0755`.
2. prod: `execFile(temp, ['-v'])` (no timeout). Reject → return
   `` `Полученный бинарь mihomo не запускается: ${msg.slice(0, 300)}` `` (success false; receiver
   unlinks temp). Why: catches broken upload and microarch mismatch that would otherwise SIGILL later (`:71-72`).
3. prod: `verifyMihomoAcceptsLiveConfig(temp)` (`:38-53`): config `REAR_MIHOMO_CONFIG_PATH`
   (`/etc/route-agent/rear.yaml`); missing → ok (binary may precede first rear config, test
   `tests/binaryReceiver.test.ts:166-177`); else `execFile(temp, ['-t','-d', dirname(cfg), '-f', cfg])`;
   error = `(stderr||message||'unknown error').slice(0,1000)`. Reject → warn, return
   `` `Бинарь mihomo ${version} отвергнут: он не принимает уже применённый конфиг ${configPath}. Прежний бинарь оставлен на месте. ${error.slice(0, 400)}` ``.
4. `replaceBinaryAtomically(temp, MIHOMO_BINARY_PATH)` (`/usr/local/bin/mihomo`). **No setcap** —
   deliberate: rear listens on 127.0.0.1:29000/29001 only (`:20-23`).
5. prod: if `stat(REAR_MIHOMO_UNIT_FILE_PATH)` (`/etc/systemd/system/route-rear-mihomo.service`) succeeds →
   `exec('systemctl restart route-rear-mihomo')` (unit name **hard-coded**), failure logged. No unit
   is created here (rear provisions it with its config, `:99-101`).
6. return `` `mihomo binary version ${version} successfully updated` `` (`version` may be `'unknown'`).
- Apply throws → receiver: `` `Failed to upload binary: ${msg}` ``.
- No version cache invalidation (mihomo version is not in telemetry).

## 7. `UploadRearRuleSet` (`mihomo.service.ts:137-203`, via receiver)

Name travels in `targetBinary`; orchestrator sends `version: ''` (`orch:GrpcBinaryUploader.ts:304-325`),
so receiver `version` stays `'unknown'` (unused). Apply(`{tempPath, targetBinary, bytes}`):

1. `SAFE_RULE_SET_NAME = /^[A-Za-z0-9][A-Za-z0-9._!-]{0,63}$/` (`:130`; `!` allowed for MetaCubeX
   `…-!cn` sets). Fail → warn (name sliced to 120), return `'Недопустимое имя набора правил.'`.
   Validation happens **before** any mkdir (test asserts the dir is not even created,
   `tests/binaryReceiver.test.ts:233-247`; rejected: `../../etc/passwd`, `..`, `a/b`, `/etc/passwd`,
   `''`, `.hidden`, 200×`a`). Orchestrator applies the same allowlist before fetching
   (`orch:githubService.ts:1012-1019`).
2. `dir = REAR_RULE_SET_DIR` (`/etc/route-agent/rules`), `mkdir -p dir`; `destPath = join(dir, name + '.mrs')`.
3. Second line of defence: `dirname(resolve(destPath)) !== resolve(dir)` → error log, same
   `'Недопустимое имя набора правил.'`.
4. `stagingPath = join(dir, '.' + name + '.mrs.' + Date.now() + '.tmp')`; `copyFile(temp, staging)`;
   `chmod staging 0644`; `rename(staging, destPath)`; finally unlink staging (ignored). Atomic because
   mihomo may read the file concurrently (`:163-164`). Test: only `category-media.mrs` remains (`:208-217`).
5. prod: `unit = basename(REAR_MIHOMO_UNIT_FILE_PATH, '.service')` (`route-rear-mihomo`);
   `exec('systemctl is-active <unit>')`, stdout trimmed `=== 'active'` (non-zero exit → false);
   if active → `exec('systemctl reload <unit>')` (SIGHUP; failure logged with name). Why reload not
   restart: all "stars" traffic goes through rear; verified mihomo re-reads on SIGHUP 2026-09-06 (`:176-186`).
6. return `` `Набор правил ${name} записан (${bytes} байт)` ``.
- Empty body → receiver `'No binary data received.'`. No content validation of `.mrs`.
- Orchestrator sends 7-ish sets sequentially and stops at the first failure
  (`orch:RearSingBoxService.ts:104-111`).

## 8. `UploadOlcrtcAgentSrvBinary` (`olcrtc.service.ts:363-490`)

Auth/receive like §1 (`'Invalid orchestrator secret token.'`, `'No binary data received.'`). Then:
1. `targetPath = OLCRTC_AGENT_SRV_BINARY_PATH` (`/usr/local/bin/olcrtc-agent-srv`); `chmod temp 0755`;
   `replaceBinaryAtomically`; unlink. No smoke test.
2. `unitJustProvisioned = await ensureOlcrtcAgentSrvSystemdUnit()` (`:97-145`, runs in test too):
   unit `OLCRTC_AGENT_SRV_UNIT_FILE_PATH` (`/etc/systemd/system/olcrtc-agent-srv@.service`), content:
   ```
   [Unit]
   Description=olcrtc-agent-srv instance %i (managed by route-agent)
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=simple
   User=root
   ExecStart=<BIN> <CONFIG_DIR>/%i.yaml
   Restart=always
   RestartSec=5

   [Install]
   WantedBy=multi-user.target
   ```
   Equal → false; else mkdir, write, prod `systemctl daemon-reload` (failure logged), true; fs error → false.
   Also called at agent startup (`src/index.ts:121`), so normally it returns false here.
3. prod && `!unitJustProvisioned`: `exec("systemctl list-units --type=service --state=running --no-legend 'olcrtc-agent-srv@*' | awk '{print $1}'")`;
   for each non-empty trimmed line `exec('systemctl restart <unit>')` sequentially (failures logged);
   enumeration failure logged. (If the unit file just changed, running instances are NOT restarted.)
4. `` `olcrtc-agent-srv binary version ${targetVersion} successfully updated` `` (test `tests/app.test.ts:476-491`).
- Catch → `` `Failed to upload binary: ${msg}` ``.

## 9. `UploadMeshAwgKernelSource` (`meshTunnel.service.ts:270-363`)

Inputs: secret, `chunk` only (`version`, `targetBinary` ignored; the orchestrator's `kernelVersion`
is never used — the version comes from `dkms.conf`).

Receive into `os.tmpdir()/amneziawg_kernel_src_<id>.tar.gz` (os.tmpdir() because the path is handed to
external tools, `:279-284`). On `end`:
1. Flush; `!secretVerified || bytesWritten===0` → unlink, `'No valid source data received or unauthorized.'`.
2. `buildDir = os.tmpdir()/amneziawg_kernel_build_<id>`; `mkdir -p`.
3. `exec('tar -xzf "<tar>" --strip-components=1 -C "<buildDir>"', {timeout:60000})`; unlink tar.
4. `installDkmsKernelModule(buildDir)` (`:208-264`):
   a. `findDkmsConf` (`:44-64`): `<b>/src/dkms.conf`, then `<b>/dkms.conf` (`fs.access`), then each
      direct subdirectory `<b>/<d>/dkms.conf` in `readdir` order; none → throw
      `` `dkms.conf not found in extracted source tarball under ${buildDir}` ``.
   b. `parseDkmsConf` (`:66-80`): `/PACKAGE_NAME\s*=\s*"([^"]+)"/`, `/PACKAGE_VERSION\s*=\s*"([^"]+)"/`;
      missing → warn and fallback `amneziawg` / `1.0.0`; `srcDir = dirname(dkms.conf)`.
   c. `isDkmsModuleInstalled` (`:184-191`): `exec('dkms status -m <name> -v <ver>')` (no timeout);
      `isDkmsInstalledForKernel(stdout, os.release())` (`:93-99`): a line counts only if the part after
      the first `:` starts with `installed` (case-insens.) and the comma-split head contains the
      **exact** running kernel release. Exec failure → false. Tests `tests/dkmsKernelStatus.test.ts:12-37`.
   d. Installed → log, skip build. Else (prod only):
      1. `removeStaleDkmsTrees(name)` (`:131-141`): `dkms status -m <name>` (30 s, failure → empty);
         for each `dkmsKnownVersions` (`:106-119`, both line formats + short `name/ver: added`):
         `dkms remove -m <name> -v <ver> --all` (60 s, failure logged); then `dpkg --configure -a`
         (300 s, failure logged). Why: half-configured `linux-headers` after a hosting kernel update made
         every `apt-get install` fail (2026-10-18, `:121-130`).
      2. `apt-get update -y` (120 s) — throws on failure.
      3. `apt-get install -y dkms build-essential linux-headers-$(uname -r)` (300 s) — throws.
      4. `patchCompatForRunningKernel(srcDir)` (`:170-182`): read `<src>/compat/compat.h` and
         `/lib/modules/<os.release()>/build/include/net/udp_tunnel.h` (either missing → no-op);
         `patchCompatForKernelHeaders` (`:153-168`) drops `#define setup_udp_tunnel_sock(` /
         `#define udp_tunnel_sock_release(` lines when the header already takes `struct sock *`
         (Ubuntu 7.0.0-38 backport); writes back only if something dropped. Tests
         `tests/dkmsKernelStatus.test.ts:70-98`.
      5. `make -C "<srcDir>" dkms-install` (60 s) — throws.
      6. `dkms add -m <name> -v <ver>` (30 s); on error, lowercased `stderr||stdout||message` must
         contain both `already` and `add` to be tolerated, else rethrow.
      7. `dkms build -m <name> -v <ver>` (180 s) then `dkms install -m <name> -v <ver>` (60 s); either
         failing → read `/var/lib/dkms/<name>/<ver>/build/make.log`, keep last 2000 chars, throw
         `` `${err.message}\n\n--- dkms build log (tail) ---\n${tail}` `` (no tail → bare message).
   e. prod (also when already installed): `modprobe amneziawg` (15 s, **hard-coded name**),
      `lsmod | grep -w amneziawg` (5 s) — both throw on failure; `mkdir -p /etc/modules-load.d`
      (ignored), write `/etc/modules-load.d/amneziawg.conf` = `"amneziawg\n"` (failure logged, best effort).
   f. `invalidateMeshAwgKernelStatusCache()` (always), return `{name, version}`.
5. Respond `` `AmneziaWG kernel module (${name} v${version}) installed and loaded` `` (test
   `tests/app.test.ts:1153-1183`: fixture `tests/fixtures/amneziawg-kernel-src-fixture.tar.gz`, top dir
   `amneziawg-linux-kernel-module-test/src/dkms.conf`, expects `amneziawg` and `9.9.9-test`).
6. Catch → `` `Failed to install kernel module: ${msg}` ``; `finally` unlink tar, `rm -rf buildDir`.

Timeouts sum to ~17.5 min worst case; orchestrator has no deadline (§0.8). The `Command failed:` +
make.log text is digested by `orch:failureDigest.ts` (§0.5). Telemetry consumer:
`getMeshAwgKernelStatus` (`telemetry.ts:356-385`: `modinfo amneziawg` fails → `not_installed`; else
`lsmod | grep -w amneziawg` → `loaded` / `built_not_loaded`).

---

## 10. Background timers / loops

None in group C. Related but owned elsewhere: AWG health timer (`telemetry.ts:603-614`, 30 s, may
restart AWG units independently of uploads), telemetry tick 2 s (reads the caches invalidated here),
rear WARP guard. Startup-only: `sanitizeAwgToolsSymlink`, `ensureOlcrtcAgentSrvSystemdUnit`
(`src/index.ts:119-121`).

## 11. Response message catalog (exact strings)

| Situation | Message |
|---|---|
| bad secret (stream, data path; also Singbox/Olcrtc/receiver at end) | `Invalid orchestrator secret token.` |
| empty (Singbox, Olcrtc, Mihomo, RuleSet) | `No binary data received.` |
| empty/unauth at end (AwgTools, AwgGo, Caddy) | `No valid binary data received or unauthorized.` |
| empty/unauth at end (Mesh) | `No valid source data received or unauthorized.` |
| generic apply failure (all but Upgrade/Mesh) | `Failed to upload binary: <msg>` |
| sing-box rejected | `Бинарь <v> отвергнут: он не принимает уже применённый на узле конфиг <path>. Прежний бинарь оставлен на месте, сервис не тронут.\n<err≤1000>` |
| sing-box ok | `sing-box binary version <v> successfully updated and restarted` |
| Upgrade | `Missing download_url in request payload.` / `Invalid download_url format.` / `Invalid URL protocol: must be http or https.` / `sing-box binary version <v> successfully upgraded from <url>` / `Failed to upgrade sing-box binary: <msg>` |
| awg-quick | `Invalid awg-quick script: missing shebang (#!) header.` / `Invalid awg-quick script syntax: <bash stderr>` |
| awg ok | `awg-quick script version <v> successfully updated<S>` / `awg-tools (awg) binary version <v> successfully updated<S>` / `amneziawg-go binary version <v> successfully updated<S>`; `<S>` ∈ {` and active AWG service(s) restarted`, ` (service restart skipped: not yet configured)`, `` (test)} |
| caddy ok | `Custom Caddy binary version <v> successfully updated and caddy.service restarted on it` (test: without suffix) |
| mihomo | `Полученный бинарь mihomo не запускается: <msg≤300>` / `Бинарь mihomo <v> отвергнут: он не принимает уже применённый конфиг <path>. Прежний бинарь оставлен на месте. <err≤400>` / `mihomo binary version <v> successfully updated` |
| rule set | `Недопустимое имя набора правил.` / `Набор правил <name> записан (<bytes> байт)` |
| olcrtc ok | `olcrtc-agent-srv binary version <v> successfully updated` |
| mesh | `AmneziaWG kernel module (<name> v<ver>) installed and loaded` / `Failed to install kernel module: <msg>` |

`<v>` defaults to `unknown` when no chunk carried a version (`latest` for UpgradeSingbox).

## 12. Parity checklist (testable)

Auth / framing
1. Valid secret only in metadata → accepted; only in first chunk's `orchestrator_secret` → accepted (`binaryReceiver.test.ts:97-112`).
2. Invalid secret with ≥1 chunk → response `{success:false,'Invalid orchestrator secret token.'}` delivered with **status OK**, stream torn down, nothing applied, no temp file left (all 8 stream RPCs).
3. Invalid metadata + first chunk without secret + later chunk with valid secret → rejected.
4. Zero chunks + bad metadata → per-RPC message from §0.2 step 4.
5. Valid secret, only empty chunks / version-only chunk → "no data" message, apply not called (`binaryReceiver.test.ts:137-149`).
6. Multi-chunk payload assembled byte-exact in order; `bytes` = total length (`binaryReceiver.test.ts:52-74`).
7. `version`/`targetBinary` = last non-empty value across chunks; default `unknown` / `''`.
8. `is_final` ignored: a stream with `is_final=true` on a middle chunk still appends later chunks; completion only on half-close.
9. Temp file gone before the response is sent, on success and failure (`binaryReceiver.test.ts:80-90`).
10. Apply exception → `Failed to upload binary: <msg>`, not a gRPC error (`binaryReceiver.test.ts:151-160`).
11. 64 KiB chunks accepted; >4 MiB single message rejected by transport (default limit).

Swap
12. `replaceBinaryAtomically` creates missing target dir, overwrites existing, final mode 0755, no `.<name>_*.tmp` left in target dir.
13. Commands with embedded errors surface as `Command failed: <cmd>\n<stderr>` in the response text.

sing-box upload
14. Order: chmod → `version` → setcap(temp) → check front cfg → check rear cfg → swap → setcap(target) → invalidate version cache → ensure unit → [enable if unit changed] → restart (always) → success.
15. Missing front/rear config is skipped; a rejecting binary yields the Russian message naming the first failing config path (front before rear); target untouched; no restart (`singboxBinaryCompat.test.ts`).
16. Restart command = `SINGBOX_RESTART_COMMAND` (default `systemctl restart sing-box`), executed even when the unit file changed; never `RELOAD_COMMAND`, never `enable --now` (`singboxBinarySwap.test.ts`).
17. Restart failure does not change the success response.
18. Unit file content byte-identical to §1 step 10; second call is a no-op without daemon-reload; no CapabilityBoundingSet/AmbientCapabilities (`app.test.ts:575-620`).
19. After success, next telemetry reports the new `singbox_version` (cache invalidated), with suffix preserved (`1.14.0-hotusers`).

AWG
20. `target_binary=='awg-quick'` → AWG_QUICK path, shebang check, `bash -n`; anything else → AWG_TOOLS path.
21. Shebang-less script rejected with exact message; syntax error rejected with `Invalid awg-quick script syntax: …`.
22. After tools/go/quick upload, every active `route-awg@*`/`awg-quick@*` unit restarted; fallback probe of `route-awg@<iface>`; suffix strings exact.
23. Startup removes a symlink at AWG_TOOLS path, leaves regular files/missing paths alone (`app.test.ts:510-535`).

Caddy
24. Target `CADDY_BINARY_PATH` or `/usr/local/bin/caddy-custom`; drop-in content byte-exact; drop-in changed → daemon-reload + restart caddy; unchanged → plain `systemctl restart caddy`; restart failure → `success:false` (binary already swapped).
25. `resolveCaddyBinary` sees the new binary immediately (no agent restart).

mihomo / rule sets
26. mihomo: `-v` smoke test, `-t -d <dir> -f <cfg>` check only if cfg exists, no setcap, restart `route-rear-mihomo` only if unit file exists.
27. Rule set name allowlist exactly `^[A-Za-z0-9][A-Za-z0-9._!-]{0,63}$`; rejected names create nothing (not even the dir); `category-ai-!cn` accepted; file `<dir>/<name>.mrs` mode 0644; directory contains no staging leftovers; reload (not restart) only when unit is `active`.

olcrtc
28. Swap → ensure template unit → if unit unchanged, restart each running `olcrtc-agent-srv@*`; if changed, restart none.

Mesh
29. Tarball extracted with `--strip-components=1`; dkms.conf search order; regex parse with fallbacks `amneziawg`/`1.0.0`; response names the parsed name+version (fixture → `amneziawg v9.9.9-test`).
30. Already-installed detection matches only the exact running kernel and state `installed`; otherwise stale-tree removal → dpkg configure → apt update → apt install → compat patch → make dkms-install → dkms add (tolerate "already…add") → build → install; build/install failure appends `--- dkms build log (tail) ---` + ≤2000 chars of make.log.
31. Exact per-command timeouts from §9; modprobe + lsmod even when already installed; modules-load.d file `amneziawg\n`; tar and build dir removed in all outcomes.
32. `isDkmsInstalledForKernel`, `dkmsKnownVersions`, `patchCompatForKernelHeaders` pass the cases in `tests/dkmsKernelStatus.test.ts`.

Upgrade
33. Unary auth/validation messages exact (`app.test.ts:876-898`); prod outcome per §2 (owner decision §13).

Lifecycle
34. Client cancel/disconnect after half-close does not abort an apply already in progress (TS behaviour; Go must not bind the apply's commands to the stream context unless the owner decides otherwise).

## 13. Risks / smells (flag, not fixed)

1. **`UpgradeSingbox` cannot succeed in prod**: `--` before `-o` makes curl treat `-o <path>` as URLs
   (verified), the URL is a tarball anyway, the swap path *reloads* (default `RELOAD_COMMAND`) instead
   of restarting, skips the live-config check and unit provisioning. The orchestrator no longer calls it.
   Decide: port as-is (always fails), port "fixed", or keep RPC returning a fixed failure.
2. **No integrity check** (size, hash, signature) on any upload; secret-holder can install arbitrary
   root binaries. Only smoke tests for sing-box/mihomo/awg-quick; none for awg, amneziawg-go, caddy, olcrtc.
3. **Caddy upload has no pre-swap validation** and restarts Caddy unconditionally; on egress nodes
   Caddy terminates VLESS (`singbox.ts:87-89`), so a bad build takes subscribers down with no rollback.
   Restart failure is reported as failure though the binary is already swapped. `run --environ` makes
   Caddy print its environment (incl. `CF_API_TOKEN` from cloudflare.env) to the journal.
4. **No rollback anywhere**: previous binaries are not kept; sing-box/mihomo rely solely on
   pre-swap checks; AWG/olcrtc/caddy have none.
5. **Unhandled WriteStream `error`** (ENOSPC/EIO in /tmp, which may be tmpfs; fork binary ~81 MB plus
   its copy) crashes the whole agent process. Writes ignore backpressure (whole binary may sit in RAM).
6. **No fsync** of binary, unit files, drop-in or rule sets before rename/restart — power loss can leave
   zero-length files. Unit/drop-in/olcrtc unit writes are non-atomic `writeFile`.
7. **No agent-side serialization**: two concurrent uploads of the same artifact race (last rename wins,
   double restart); rule-set staging name `.<name>.mrs.<ms>.tmp` can collide within one ms. Orchestrator
   dedupe is per-process only.
8. **Apply survives cancellation** but cleanup on `cancelled` may delete the temp under it → spurious
   ENOENT failure after partial side effects (e.g. sing-box already checked, not swapped).
9. `/tmp` mounted `noexec` would make every smoke test fail (`EACCES`) → all sing-box/mihomo uploads rejected.
10. `setcap` on the temp file is pointless (lost on copy); `setcap` failures are silent. sing-box unit runs as root anyway.
11. AWG tools/go upload restarts **all** active `route-awg@*` units, including the S2S mesh interface `awgmesh0` → mesh blip on every AWG tools delivery (mesh provisioning itself delivers tools first).
12. Suspected (not verified with real binaries): reverting **fork → stock** sing-box may be blocked by
    `verifyBinaryAcceptsLiveConfigs`, because the live config contains the agent-injected
    `services:[{type:'users-api'}]` (`hotUsers.ts:77-86`) which a stock build likely rejects as an
    unknown service type. Worth testing on a node before relying on the Go port to behave "better".
13. sing-box success message says "restarted" even when the restart command failed; restart/enable/daemon-reload/setcap/systemctl failures are log-only across handlers.
14. Mesh: `modprobe`/`lsmod` use hard-coded `amneziawg` regardless of parsed `PACKAGE_NAME`; payload `version` ignored; `exec` timeouts SIGTERM only the direct child (pipelines/`$(uname -r)` subshells may orphan apt); apt output over 1 MiB on stdout would kill the command via maxBuffer.
15. Four copies of the receive loop remain un-migrated to `binaryReceiver` (`binaryReceiver.ts:25-28`), which is why messages differ per RPC (§0.2 step 4). A Go port that unifies them must still emit the per-RPC strings.
16. `is_final` is dead on the wire; proto comment on Mesh misdescribes it.
17. Receiving into memory-unbounded, size-unbounded files: no maximum; a runaway stream fills /tmp.

## 14. Undetermined

- Whether stock sing-box `check` rejects the `users-api` service block (risk 12) — needs a real binary.
- Exact ordering guarantees of grpc-js between the `callback` response and `call.destroy()` on the
  unauthorized path are relied on only via tests (status OK observed); Go should send the response
  and return (OK status) — confirmed sufficient by the orchestrator, which only reads `success`.
- Whether any node runs with `CADDY_BINARY_PATH` set in `.env` (affects only the path, both code paths read it).
