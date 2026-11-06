# Spec A: process, transport, security, lifecycle

This is the behaviour spec for the Go rewrite of `route-agent`. It covers group A: process model, configuration, gRPC transport and mTLS, authentication, logging, shutdown, the `GetAgentInfo`, `SelfUpdate` and `GetManagedCertificate` RPCs, the shared helpers (`exec`, `certStorage`, `plannedWrites`, `systemdUnit`), the boot-time loops, and `install.sh` and `uninstall.sh`.

Sources read: route-agent at branch `go-rewrite` (HEAD `a6b1055`) and the orchestrator `/home/user/vpn-orchestrate` (its only gRPC client). Citation forms:
- `src/…:N` means route-agent.
- `orch:src/…:N` means the orchestrator.

All line numbers refer to the state as of 2026-11-06.

Conventions:
- **MUST** means the orchestrator or existing boxes depend on the behaviour.
- **SHOULD** means the behaviour is observable but nothing is known to depend on it.
- "Wire name" is the camelCase proto-loader name (`keepCase: false`). The proto file itself uses snake_case.

---

## 0. Inventory of what this document covers

| Kind | Item | Where |
|---|---|---|
| Entry point and boot | `startServer`, `getGrpcServerCredentials`, top-level start | `src/index.ts:1-229` |
| Config | env schema (33 variables) | `src/config.ts:1-105` |
| Auth | `verifySecret`, `extractSecretFromMetadata`, `authenticateCall` | `src/middleware/auth.ts:1-39` |
| RPC | `GetAgentInfo` | `src/services/system.service.ts:62-89` |
| RPC | `SelfUpdate` | `src/services/system.service.ts:94-120` |
| RPC | `GetManagedCertificate` | `src/services/config.service.ts:455-487` |
| Module | proto contract hash | `src/services/protoContract.service.ts:1-113` |
| Module | AWG systemd unit | `src/services/systemdUnit.service.ts:1-62` |
| Module | exec wrappers | `src/utils/exec.ts:1-5` |
| Module | Caddy cert storage paths | `src/utils/certStorage.ts:1-29` |
| Module | planned writes | `src/utils/plannedWrites.ts:1-70` |
| Boot helpers (owned by other modules, called at boot) | `sanitizeAwgToolsSymlink`, `ensureOlcrtcAgentSrvSystemdUnit`, `fixXraySocketPermissions`, `startAwgHealthCheckTimer`, `applyKernelTuning`, `startRearWarpGuard` | see §10 |
| Install | `install.sh`, `uninstall.sh` | repo root |
| Logging | pino setup | 35 files, `pino({ level: 'info' })` |

RPCs covered in depth: 3 (`GetAgentInfo`, `SelfUpdate`, `GetManagedCertificate`). The auth behaviour of all 31 RPCs is in the matrix in §4.3.

---

## 1. Process model and boot sequence

### 1.1 How the process is started on a box

- systemd unit `/etc/systemd/system/route-agent.service`, written by `install.sh:279-295`, verbatim:
  ```ini
  [Unit]
  Description=Route Egress gRPC Agent Service
  After=network.target

  [Service]
  Type=simple
  User=root
  WorkingDirectory=/opt/route-agent
  ExecStart=<command -v node> /opt/route-agent/dist/index.js
  Restart=always
  RestartSec=5
  EnvironmentFile=/opt/route-agent/.env

  [Install]
  WantedBy=multi-user.target
  ```
  - `<command -v node>` is resolved at install time (`install.sh:277`). It is `/usr/bin/node` for NodeSource and `/usr/local/bin/node` for the mirror install.
  - The process runs as **root**. Its cwd is `/opt/route-agent`, and several things are resolved relative to cwd: the proto file, `package.json`, and `git describe` (§7).
  - No `KillMode`, `TimeoutStopSec`, `LimitNOFILE` or `Environment=NODE_ENV` is set, so systemd defaults apply: `KillMode=control-group`, SIGTERM then SIGKILL after 90 s, and `NODE_ENV` unset.
- `Restart=always` + `RestartSec=5`: with 5 s between starts, the default `StartLimitBurst=5` within `StartLimitIntervalSec=10s` can never trigger. A startup failure therefore produces an **infinite restart loop every 5 s**, and `index.ts:226` says so explicitly in its fatal log.

### 1.2 Module-load side effects (before `startServer`)

The order is the ESM import order of `src/index.ts:1-46`. Anything at module top level runs at import time:
1. `config.ts`:
   - `dotenv.config()` (`src/config.ts:4`) reads `./.env` relative to cwd. It **does not override** variables already in the environment.
   - Zod `safeParse(process.env)` (`:98`). On failure it writes `'❌ Invalid agent environment variables: ' + JSON.stringify(error.format(), null, 2) + '\n'` to **stderr** (plain text, not pino JSON) and calls `process.exit(1)` (`:99-102`).
2. `index.ts:48`: `const logger = pino({ level: 'info' })`.
3. `index.ts:50-60`: `protoLoader.loadSync(path.resolve(process.cwd(), 'proto/agent.proto'), {keepCase:false, longs:String, enums:String, defaults:true, oneofs:true})`.
   - It runs synchronously at import.
   - A missing proto throws an **uncaught exception** at import: a Node stack trace and exit code 1, *not* the "💥" fatal log.
4. Every service module creates its own `pino({ level: 'info' })` (35 instances). Several also create module-level state; for example, `telemetry.ts` has `awgHealthStates`.

### 1.3 `startServer()` (`src/index.ts:118-221`)

The steps run strictly sequentially, each awaited:

| # | Call | File | Failure behaviour |
|---|---|---|---|
| 1 | `sanitizeAwgToolsSymlink()` | `src/services/binary.service.ts:282-297` | Never throws (all errors swallowed) |
| 2 | `ensureAwgSystemdUnit()` | `src/services/systemdUnit.service.ts:14-62` | Never throws (logs error, returns false) |
| 3 | `ensureOlcrtcAgentSrvSystemdUnit()` | `src/services/olcrtc.service.ts:97-145` | Never throws |
| 4 | `fixXraySocketPermissions()` | `src/utils/caddy.ts:180-196` | Never throws |
| 5 | `startAwgHealthCheckTimer()` (sync) | `src/utils/telemetry.ts:603-614` | n/a. Note the timer is started **before** the credentials check. |
| 6 | `await getGrpcServerCredentials()` | `src/index.ts:75-116` | **Throws**, so the agent exits 1 (§3.2) |
| 7 | `new Server(serverOptions)`, `addService`, `bindAsync(HOST:PORT)` | `src/index.ts:125-219` | Bind error: `logger.error({err}, 'Failed to bind gRPC server')`, reject, then exit 1 |
| 8 | On bind success: compute the proto hash, log the listening line (§5), set `serverInstance` | `:190-192` | |
| 9 | If `NODE_ENV !== 'test'`: `void applyKernelTuning()` (**not awaited**), then `startRearWarpGuard()` | `:201-216` | See §10.5 and §10.6 |

If `NODE_ENV !== 'test'`, the top level calls `startServer().catch(...)`. The catch logs `logger.fatal({ err: msg }, '💥 Route Agent failed to start and is exiting; systemd (Restart=always) will retry, but this will loop until the underlying issue is fixed')` and then calls `process.exit(1)` (`src/index.ts:223-229`).

`serverInstance` (`:62`) is exported and assigned but never read anywhere in `src/`. Only tests call `forceShutdown()` on the returned server.

### 1.4 Shutdown and signals

- **There is no graceful shutdown.** No `process.on('SIGTERM'|'SIGINT'|'SIGHUP')`, no `server.tryShutdown`, no `uncaughtException` or `unhandledRejection` handler. (`grep` over `src/`: the only SIGTERM references are `journalProcess.kill('SIGTERM')` for spawned journalctl children in `telemetry.service.ts:72` and `olcrtc.service.ts:293`.)
- Effect under systemd stop or restart:
  - Node's default SIGTERM disposition terminates immediately.
  - In-flight RPCs are cut: clients see `UNAVAILABLE` or `ECONNRESET`.
  - Child processes (journalctl followers, a running `npm ci` from SelfUpdate, and so on) are killed by systemd's control-group kill.
  - Temp files in `/tmp` (upload `*.tmp`) may be left behind.
- Unhandled promise rejection: the Node 22 default is `--unhandled-rejections=throw`, so the process dies and systemd restarts it 5 s later. Any handler that throws outside its own try/catch therefore crashes the whole agent. Known instance: `void applyKernelTuning()` (§10.5).
- The SelfUpdate pipeline ends with `systemctl restart route-agent` executed **by a child of the agent itself** (§8). systemd kills the agent, including that shell, but the restart job is already queued in systemd, so the restart completes.

**Go guidance.** Reproducing "SIGTERM ⇒ exit promptly" is sufficient. A Go `GracefulStop` with a short bound (≤ a few seconds) is acceptable as long as stop never hangs on the long-lived `StreamTelemetry` and `StreamOlcrtcEvents` streams. They never end on their own, so use `Stop()` after a timeout.

---

## 2. Configuration (`src/config.ts`)

### 2.1 Loading semantics

- **Sources:**
  - systemd `EnvironmentFile=/opt/route-agent/.env`.
  - `dotenv.config()` loads `.env` from **cwd** (also `/opt/route-agent/.env` under systemd), without overriding. In practice that is the same file loaded twice; dotenv only matters for manual `npm start` or `npm run dev`.
- **Validation:**
  - `z.object({...})` with `.safeParse(process.env)`. Unknown variables are **silently stripped**. For example, `OLCRTC_BINARY_PATH` and `OLCRTC_MANAGER_BINARY_PATH` from `.env.example` are ignored.
  - `z.string()` accepts the **empty string**.
  - `z.coerce.number()` converts with `Number(x)`:
    - `""` becomes `0`, which is **valid**.
    - `"abc"` becomes `NaN`, which is rejected.
    - There is no integer or range check.
  - `.default(x)` applies only when the variable is **absent**, not when it is empty (see the comment at `src/config.ts:59-67`).
- **On failure:** stderr text plus exit 1 (§1.2).
- The `config` object is a module singleton and is never reloaded. Tests mutate it in place (`tests/app.test.ts:1695-1698`).

### 2.2 Every variable

The Line column is the line in `src/config.ts`. The type is `string` unless stated otherwise.

| Variable | Line | Default | Consumers | Notes |
|---|---|---|---|---|
| `PORT` | 7 | `8081` (coerced number) | `index.ts:183` | gRPC listen port. `PORT=` (empty) means 0, so the agent binds a random port and silently becomes unreachable. Install writes it explicitly. |
| `HOST` | 8 | `0.0.0.0` | `index.ts:183` | Bind host. `0.0.0.0` is IPv4 only. |
| `EGRESS_CONTROL_SECRET` | 9 | **required** | `middleware/auth.ts:13` | Shared secret: 64 hex chars from the orchestrator (`orch:src/core/services/nodeOps/agentInstall.ts:60`). The empty string passes Zod, but `verifySecret` then rejects every call (fail closed). |
| `SINGBOX_CONFIG_PATH` | 10 | `/etc/sing-box/config.json` | binary.service, utils/singbox, singboxConnections, listenerWarning | Install writes it explicitly. |
| `SINGBOX_BINARY_PATH` | 11 | `/usr/local/bin/sing-box` | binary.service, rearCore, singbox, telemetry | |
| `SINGBOX_UNIT_FILE_PATH` | 12 | `/etc/systemd/system/sing-box.service` | singbox, telemetry, journalWarnings | |
| `RELOAD_COMMAND` | 13 | `systemctl reload sing-box` | binary.service, singbox | Shell string. Install writes it explicitly. Also read raw as `process.env.RELOAD_COMMAND` at `utils/singbox.ts:325` (test switch). |
| `SINGBOX_RESTART_COMMAND` | 25 | `systemctl restart sing-box` | binary.service, singbox | Shell string. Rationale comment at `:14-24`: a reload keeps the old image in memory (incident 2026-09-20, `aeza-msc-1`). |
| `REAR_SINGBOX_CONFIG_PATH` | 35 | `/etc/route-agent/rear.json` | binary.service, warpKeyHealth.service, rearCore | Must NOT live in `/etc/sing-box`: the packaged unit uses `-C /etc/sing-box`, which merges every `*.json` (`:26-34`, incident 2026-09-04 on `mo-nl-node`). |
| `REAR_SINGBOX_UNIT_FILE_PATH` | 36 | `/etc/systemd/system/route-rear-singbox.service` | rearCore, journalWarnings | |
| `MIHOMO_BINARY_PATH` | 42 | `/usr/local/bin/mihomo` | mihomo.service, rearCore | Separate path on purpose (`:38-41`). |
| `REAR_MIHOMO_CONFIG_PATH` | 43 | `/etc/route-agent/rear.yaml` | mihomo.service, rearCore | |
| `REAR_MIHOMO_UNIT_FILE_PATH` | 44 | `/etc/systemd/system/route-rear-mihomo.service` | mihomo.service, rearCore | |
| `REAR_RULE_SET_DIR` | 46 | `/etc/route-agent/rules` | mihomo.service | The orchestrator hard-codes the same path (`orch:src/core/generators/rearMihomo.ts:116`). They MUST match. |
| `REAR_WARP_GUARD_INTERVAL_MS` | 50 | `30000` (number) | rearWarpGuard.service:86 | Empty means 0, and `setInterval(fn, 0)` means a 1 ms busy loop (risk R-12). |
| `SYSCTL_PROFILE_PATH` | 53 | `/etc/sysctl.d/99-route-agent.conf` | kernelTuning | |
| `NODE_WARNING_LOOKBACK_HOURS` | 57 | `24` (number) | nodeWarnings.service, journalWarnings | |
| `CADDYFILE_PATH` | 58 | `/etc/caddy/Caddyfile` | config.service, caddy | |
| `CADDY_RELOAD_COMMAND` | 68 | `systemctl reload caddy \|\| systemctl restart caddy` | config.service | The `\|\| restart` fallback is load-bearing and MUST live in the default itself (`:59-67`; test `tests/app.test.ts:1875-1889`). |
| `CADDY_BINARY_PATH` | 72 | *(optional, no default)* | binary.service, caddy | Code falls back to `/usr/local/bin/caddy-custom` if that file exists, else to `caddy` on `$PATH` (`utils/caddy.ts:24-32`). |
| `OLCRTC_AGENT_SRV_BINARY_PATH` | 78 | `/usr/local/bin/olcrtc-agent-srv` | olcrtc.service, telemetry | |
| `OLCRTC_AGENT_SRV_UNIT_FILE_PATH` | 79 | `/etc/systemd/system/olcrtc-agent-srv@.service` | olcrtc.service | |
| `OLCRTC_AGENT_SRV_CONFIG_DIR` | 80 | `/etc/olcrtc-agent-srv` | olcrtc.service | |
| `AWG_CONFIG_PATH` | 81 | `/etc/amnezia/amneziawg/awg0.conf` | config.service, telemetry, awg | The interface name is `basename(path, '.conf')`, or `awg0` if that is empty (`utils/awg.ts:12-16`). |
| `AWG_RELOAD_COMMAND` | 82 | `systemctl restart route-awg@awg0` | **none (dead)** | Declared but never read. |
| `AWG_TOOLS_BINARY_PATH` | 83 | `/usr/local/bin/awg` | binary.service, systemdUnit.service, telemetry | The AWG unit derives `awg-quick` as `${this}-quick` (§9.3). |
| `AWG_QUICK_BINARY_PATH` | 84 | `/usr/local/bin/awg-quick` | binary.service only | **Not** used by the systemd unit (risk R-14). |
| `AWG_GO_BINARY_PATH` | 85 | `/usr/local/bin/amneziawg-go` | binary.service, telemetry | |
| `AWG_UNIT_FILE_PATH` | 86 | `/etc/systemd/system/route-awg@.service` | systemdUnit.service | |
| `MESH_AWG_CONFIG_PATH` | 92 | `/etc/amnezia/amneziawg/awgmesh0.conf` | meshTunnel.service, awg | |
| `CA_CERT_PATH` | 93 | `/etc/route-agent/certs/ca.crt` | index.ts:76 | Install writes it explicitly. |
| `AGENT_CERT_PATH` | 94 | `/etc/route-agent/certs/agent.crt` | index.ts:76 | Install writes it explicitly. |
| `AGENT_KEY_PATH` | 95 | `/etc/route-agent/certs/agent.key` | index.ts:76 | Install writes it explicitly. |

Other environment reads outside the schema:
- `NODE_ENV`: 42 sites. `'test'` disables systemctl and exec side effects, boot loops, and so on.
- Test hooks: `TEST_*`, `*_TEST_*`.
- `process.env` is passed whole to children:
  - `caddy validate` (`config.service.ts:400`) gets `{...process.env, ...cfEnv}`.
  - Every `execAsync` child inherits the environment, including `EGRESS_CONTROL_SECRET` (risk R-9).

**`.env` that `install.sh` writes** (`install.sh:264-274`, mode `0600`, owner root, path `/opt/route-agent/.env`):
```
PORT=<--port, default 8081>
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=<secret from the bundle, or the raw --secret>
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
CA_CERT_PATH=/etc/route-agent/certs/ca.crt
AGENT_CERT_PATH=/etc/route-agent/certs/agent.crt
AGENT_KEY_PATH=/etc/route-agent/certs/agent.key
```
Everything else comes from schema defaults. The Go agent MUST:
- accept this exact file, including unquoted values with spaces (`RELOAD_COMMAND=systemctl reload sing-box`);
- apply the same defaults for absent variables;
- ignore unknown keys.

---

## 3. Transport: gRPC server, mTLS, keepalive, limits

### 3.1 Server

- **Library:** `@grpc/grpc-js` 1.14.4 with `@grpc/proto-loader` 0.7.15 and protobufjs 7.6.5 (`package-lock.json:441-471,1627`).
- **Service:** `agent.EgressAgentService` from `proto/agent.proto`. All 31 RPCs are mapped (`src/index.ts:147-179`), so no method returns `UNIMPLEMENTED`. Unknown methods return `UNIMPLEMENTED (12)`.
  - The orchestrator relies on code 12 from `GetAgentInfo` to classify very old agents (`orch:src/core/services/protoContractVerifier.ts:283`).
- **Loader options** (`src/index.ts:51-57`): `keepCase:false, longs:String, enums:String, defaults:true, oneofs:true`.
  - The wire names are the proto's snake_case names, but handlers see camelCase JS keys.
  - Request objects have proto3 defaults filled in (`''`, `0`, `false`, `[]`).
  - **Response objects:** keys not in the schema in camelCase form are **silently dropped**. Handlers that set both `proto_contract_hash` and `protoContractHash` only get the camelCase one serialized (`tests/responseFieldNaming.test.ts:11-31`). For Go this is irrelevant (generated structs), but it means the snake_case twins in handler code are not behaviour.
- **Bind:** `server.bindAsync('${HOST}:${PORT}', credentials, cb)` (`src/index.ts:183-219`). The default is IPv4 `0.0.0.0:8081`. The orchestrator default port is also 8081 (`orch:src/core/services/nodeOps/agentInstall.ts:26`).
- **Server options** (`src/index.ts:126-145`), exactly:

  | Option | Value | Effect in grpc-js 1.14 (server side) |
  |---|---|---|
  | `grpc.keepalive_time_ms` | `10000` | The server PINGs every HTTP/2 session every 10 s, **regardless of activity** (`node_modules/@grpc/grpc-js/build/src/server.js:1121-1180`) |
  | `grpc.keepalive_timeout_ms` | `20000` | No PING ack within 20 s destroys the session. Comment `:128-140`: was 5 s and changed 2026-09-18. **Mirrored on the orchestrator client** (`orch:src/core/services/grpc/GrpcConnectionManager.ts:294-307`), so both ends MUST change together. |
  | `grpc.keepalive_permit_without_calls` | `1` | Client-side option, no effect on a grpc-js server |
  | `grpc.http2.min_ping_interval_without_data_ms` | `5000` | **Ignored by the grpc-js server.** It has no ping-strike enforcement, so the agent tolerates any client ping rate. |
  | `grpc.http2.max_pings_without_data` | `0` | Client-side option, no effect |

- **Message size limits:**
  - Not configured, so the grpc-js defaults apply: **max receive 4 MiB** (`DEFAULT_MAX_RECEIVE_MESSAGE_LENGTH = 4*1024*1024`) and **max send unlimited** (`-1`).
  - The orchestrator also sets no limits. It streams binaries in **64 KiB chunks** (`orch:src/core/services/grpc/GrpcBinaryUploader.ts:153`).
  - A unary request over 4 MiB (for example a huge `ApplyConfig.config_json`) gets `RESOURCE_EXHAUSTED` from the transport.
  - No `max_concurrent_streams` or `max_connection_idle`/`age` options are set (unlimited).

**Go mapping (MUST):**
- `keepalive.ServerParameters{Time: 10*time.Second, Timeout: 20*time.Second}`.
- **`keepalive.EnforcementPolicy{MinTime: ≤5*time.Second (or lower), PermitWithoutStream: true}`.**
  - The Go default (`MinTime` 5 min, `PermitWithoutStream` false) would send `GOAWAY too_many_pings` to the orchestrator.
  - The orchestrator client pings every 10 s with `keepalive_permit_without_calls: 1` (`orch:…/GrpcConnectionManager.ts:294-308`).
  - The Node agent never enforced this, so the Go agent must be at least as permissive.
- `grpc.MaxRecvMsgSize(4<<20)` is the Go default, which matches. The Go default max send is `math.MaxInt32`, which is fine.

### 3.2 mTLS credentials (`getGrpcServerCredentials`, `src/index.ts:75-116`)

Steps:
1. Run `fs.stat` on `CA_CERT_PATH`, `AGENT_CERT_PATH` and `AGENT_KEY_PATH` in parallel. Each result is just "exists or not".
2. If **all three exist**, read all three (raw bytes) and log `logger.info({ caPath, certPath }, '🔒 Enabling mTLS with strict client certificate verification')`.
3. Return `ServerCredentials.createSsl(caCert, [{cert_chain: agentCert, private_key: agentKey}], true)`. The third argument `checkClientCertificate=true` means the client cert is **required and verified against the CA**.
4. If reading throws (for example `EISDIR` because the key path is a directory), throw:
   `mTLS certificates were found but failed to load (CA: <p>, cert: <p>, key: <p>). Underlying error: <msg>. Agent refuses to start in an unencrypted/unauthenticated transport mode. …` (`:93-101`).
   - Note: invalid PEM *content* is not detected here. `createSsl` does not parse eagerly; a bad PEM fails later at `bindAsync` (which rejects, so the agent exits 1) or at handshake time.
5. If any file is missing, throw:
   `mTLS certificates are required but unavailable (CA: ok|MISSING at <p>, cert: …, key: …). Agent refuses to start in an unencrypted/unauthenticated transport mode instead of silently degrading (x-orchestrator-secret must never travel in plaintext). …` (`:105-115`).
6. **There is no insecure fallback, by design** (`:64-74`: "silent degradation to createInsecure() would send x-orchestrator-secret in plaintext … fail closed"). Tests: `tests/app.test.ts:1761-1820` match the regexes `/mTLS certificates are required but unavailable/`, `/MISSING at/`, `/refuses to start/` and `/mTLS certificates were found but failed to load/`.

Certificates are loaded **once at startup**. Replacing the files requires a process restart, which is exactly what `install.sh` does.

**TLS semantics the orchestrator depends on.** Client side (`orch:src/core/services/grpc/GrpcConnectionManager.ts`):
- **Server identity.** The orchestrator does not do normal hostname verification. It overrides `checkServerIdentity` (`:49-52`, `:81-108`):
  1. The server cert's **subject CN MUST equal the node UUID**. `ssl_target_name_override` and `default_authority` are set to the node UUID (`:317-320`), so the TLS **SNI is the node UUID** and the `:authority` is the UUID.
  2. If a fingerprint is pinned (DB column `agent_cert_fingerprint`), the SHA-256 of the presented **leaf DER**, rendered `AB:CD:…` upper-case, MUST equal the pin.
  - Consequence for Go: serve exactly the leaf in `agent.crt`, byte for byte (the DER is unchanged by Go's PEM decoding). The cert has **no SAN**; the Go *server* doesn't care.
- **Node certificate format** (as issued by `orch:src/shared/utils/certs.ts:240-270`):
  - ECDSA P-256 key, private key PEM in **PKCS#8** (`-----BEGIN PRIVATE KEY-----`, `:64-66`);
  - subject `CN=<nodeUuid>, O=Route Orchestrator Node`;
  - EKU `serverAuth` only;
  - KU `digitalSignature`, `keyEncipherment`;
  - 10-year validity;
  - signed with ecdsa-with-SHA256 by `CN=Route Orchestrator Root CA, O=Route Orchestrator`.
  - Older boxes may carry certs from before the EC switch (git history is too shallow to confirm). The Go agent SHOULD accept any key format `tls.X509KeyPair` accepts (PKCS#1 or PKCS#8 RSA, SEC1 or PKCS#8 EC).
- **Client certificate:** `CN=Route Orchestrator Client`, EKU `clientAuth` (`orch:src/shared/utils/certs.ts:200-217`).
  - Node/OpenSSL on the server verifies the client chain against `ca.crt` with the SSL-client purpose. A cert with an EKU that lacks `clientAuth`, such as another node's server cert, is rejected.
  - Go's `tls.RequireAndVerifyClientCert` with `ClientCAs` verifies with `ExtKeyUsageClientAuth`, which is equivalent.
  - **No identity check on the client cert beyond the chain**: any CA-signed cert with a suitable EKU is accepted.
- **Connection pattern:**
  - The orchestrator builds a **new channel per unary RPC** (`getClient` → call → `client.close()`; `orch:…/GrpcConfigPusher.ts:113-138`).
  - It uses `grpc.use_local_subchannel_pool: 1` (`GrpcConnectionManager.ts:314`), which means one TCP+TLS handshake per call. The Go server must tolerate many short-lived connections.
  - Long-lived connections: `StreamTelemetry` and `StreamOlcrtcEvents`.
- **TLS versions:**
  - Node defaults to a minimum of TLS 1.2 and a maximum of TLS 1.3. ALPN `h2` is negotiated by grpc-js. Go's grpc credentials require ALPN `h2`; grpc-js clients send it.
- **Test fixtures:** `tests/fixtures/certs/`:
  - `ca.crt` with `CN=TestRootCA`;
  - `agent.crt` with `CN=127.0.0.1`, SAN `IP:127.0.0.1, DNS:localhost`;
  - `client.crt` with `CN=TestClient`;
  - `untrusted.crt`, signed by a separate `UntrustedCA`.
  - All keys are PKCS#8. Behaviour (`tests/app.test.ts:1685-1759`): a valid client succeeds, and an untrusted client fails at the handshake (the call returns an error).

### 3.3 Client-side timeouts, retries and their implications for the agent

These are orchestrator facts the agent's timing must fit inside:

| What | Value | Source |
|---|---|---|
| Default unary deadline | 60 s | `orch:…/GrpcConfigPusher.ts:62` |
| "Observational" RPCs (GetAgentInfo, connections, traffic, warnings…) | 10 s | `:80`, `:257` |
| `SelfUpdate`, `UpgradeSingbox` | 10 min | `:87`, `:787` |
| Retry | Exactly one retry, only if the request **never left** (`DEADLINE_EXCEEDED` + "Waiting for LB pick", or `UNAVAILABLE` + "No connection established") | `orch:src/shared/utils/grpcNeverSent.ts:17-23`, `GrpcConfigPusher.ts:97-108` |
| Wait for agent to return after SelfUpdate | 180 s | `orch:src/core/services/protoContractVerifier.ts:35,323-334` |

---

## 4. Authentication (`src/middleware/auth.ts`)

### 4.1 Functions

- `verifySecret(token)` (`:7-18`):
  1. Returns false if the token is not a non-empty string.
  2. `expected = config.EGRESS_CONTROL_SECRET || ''`.
  3. Encodes both as UTF-8 buffers.
  4. Returns false if the byte lengths differ **or** the expected value is empty.
  5. Otherwise returns `crypto.timingSafeEqual`.
  - Length is leaked by the early return, which is acceptable.
  - Test (`tests/app.test.ts:1827-1833`): `''`, `null`, `undefined` and wrong values give false.
- `extractSecretFromMetadata(call)` (`:23-26`): `call.metadata.get('x-orchestrator-secret')`, takes the **first** value, coerces with `String()`, and returns `''` if absent.
  - The metadata key is case-insensitive on the wire; gRPC lowercases it.
- `authenticateCall(call)` (`:31-39`): metadata first, then `call.request.orchestratorSecret || call.request.orchestrator_secret`. The second form is dead, since there are no snake keys with `keepCase:false`.
  - Client-streaming calls have no `call.request`, so it falls back to metadata only.

### 4.2 What the orchestrator sends

- **Always** metadata `x-orchestrator-secret: <node secret>` (`orch:…/GrpcConnectionManager.ts:329-333`, used for every call).
- Additionally a body `orchestratorSecret` on RPCs whose request has the field, including every upload chunk (`orch:…/GrpcBinaryUploader.ts:162-168`).
- `GetManagedCertificate` is sent with `{ domain }` only, relying on metadata (`orch:…/GrpcConfigPusher.ts:331`).

### 4.3 Auth matrix (all 31 RPCs)

Failure shapes are **MUST**: tests assert the exact `message` string, and the orchestrator treats `success:false` and gRPC errors differently (§4.4).

The four auth modes:

| Mode | Rule |
|---|---|
| **M** | Metadata only: `verifySecret(extractSecretFromMetadata(call))` |
| **M\|B** | `authenticateCall`: metadata OR body field |
| **M\|C** | Client stream: metadata, else the secret in the **first** received chunk. If the first chunk carries no valid secret, the call is aborted immediately; a later chunk cannot rescue it. |
| **none** | No authentication |

Failure shapes:

| RPC | Auth | Failure response (status `OK` unless noted) |
|---|---|---|
| ApplyConfig | M | `{success:false, message:'Invalid orchestrator secret token.'}` (`config.service.ts:103-108`) |
| ConfigureAwg | M | same (`config.service.ts:501-505`) |
| ConfigureCaddy | M\|B | same (`config.service.ts:242-245`) |
| GetManagedCertificate | M\|B | same (`config.service.ts:459-462`) |
| ManageFirewall | M | same (`firewall.service.ts:15-19`) |
| SelfUpdate | M (request is empty) | same (`system.service.ts:98-102`) |
| UpgradeSingbox | M | same (`binary.service.ts:306-310`) |
| SyncOlcrtcInstance | M | same (`olcrtc.service.ts:156-160`) |
| DeleteOlcrtcInstance | M | same (`olcrtc.service.ts:229-233`) |
| ConfigureMeshTunnel | M | same (`meshTunnel.service.ts:383-387`) |
| ConfigureRearSingbox | M\|B | `{success:false, message:'Invalid orchestrator secret token.', running:false}` |
| ApplyTrafficThrottle | M\|B | `{success:false, message:…, changed:false, applied:0, counters:[]}` |
| GetSingBoxUserTraffic | M\|B | `{success:false, message:…, entries:[]}` |
| GetSingBoxConnections, GetRearConnections | M\|B | `{success:false, message:…, connections:[]}` |
| GetWarpKeyHealth | M\|B | `{success:false, message:…, entries:[]}` |
| PullNodeWarnings, GetAwgObservation | M\|B | `{success:false, message:'Invalid orchestrator secret token.', …}` |
| RunNetworkDiagnostic | M\|B | `{success:false, message:…, results:[]}` |
| GetTelemetry | M\|B | A full telemetry frame with sentinels: `systemLogs:'Error: Unauthorized secret token mismatch'`, `webrtcStatus:'unauthorized'`, `awgStatus:'unauthorized'`, `singboxVersion:'unknown'`, … (`telemetry.service.ts:151-170`) |
| StreamTelemetry | M\|B | **gRPC error** code `16 UNAUTHENTICATED`, details `'UNAUTHENTICATED: Invalid orchestrator secret token.'`, emitted via `call.emit('error')` (`telemetry.service.ts:29-34`) |
| StreamOlcrtcEvents | M\|B | **gRPC error** `16`, same text (`olcrtc.service.ts:274-279`); test `tests/app.test.ts:1022-1044` requires `error` and no `data`/`end` |
| UploadSingboxBinary, UploadAwgToolsBinary, UploadAwgGoBinary, UploadCaddyBinary (`binary.service.ts`), UploadOlcrtcAgentSrvBinary (`olcrtc.service.ts:367-392`), UploadMeshAwgKernelSource (`meshTunnel.service.ts:274-304`), UploadMihomoBinary and UploadRearRuleSet (via `binaryReceiver.ts:61-120`) | M\|C | `callback(null,{success:false, message:'Invalid orchestrator secret token.'})` **then** `call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'))`. The response is sent first, so the client normally receives `success:false` with OK; tests accept either an error or `success:false` (`tests/app.test.ts:429-456`). If the stream ends while still unverified, the temp file is unlinked first. |
| GetAgentInfo | **none** | Never fails on auth (§7) |

**Go recommendation for the upload rejection:** return `success:false` with status OK, deterministically. Do not reproduce the racy destroy-after-callback.

### 4.4 How the orchestrator reacts to the failure shapes

- Unary `{success:false}` with status OK:
  - It is returned to the caller as-is (`executeAgentRpc` with `onError:'returnFailure'`), or thrown as `Error(message)` for `onError:'throw'` call sites, where the message is surfaced to admins.
  - No orchestrator code compares against `'Invalid orchestrator secret token.'` (grep: no matches in `orch:src`). The **tests** in this repo do compare it exactly.
- A gRPC error is formatted as `gRPC Failure [Code N]: <details>` or `gRPC error: <msg>` (`orch:…/GrpcConfigPusher.ts:205-235`).
- A `success !== false` response revives the telemetry stream and heals a stale `unreachable` contract status (`:194-205`; `orch:…/contractGuard.ts:76-91`).

---

## 5. Logging

- **Library:** pino 9.14.0. Each module creates its own `pino({ level: 'info' })`, with no transport and no pretty printing in production (`pino-pretty` is a devDependency only).
- **Destination:**
  - stdout as newline-delimited JSON, captured by journald under unit `route-agent`. There is no log file and no rotation config.
  - **Exception:** a config validation failure writes plain text to **stderr** (`src/config.ts:100`).
- **Line shape:** `{"level":30,"time":<epoch ms>,"pid":<pid>,"hostname":"<host>", …mergedObject, "msg":"…"}`. Levels: 20 debug (never emitted, since the level is info), 30 info, 40 warn, 50 error, 60 fatal.
- **The level is not configurable.** There is no `LOG_LEVEL` variable, and all `logger.debug(...)` calls are dead in production.
- Error objects are mostly logged as strings under `err` (`{ err: msg }`). One exception: `index.ts:186` logs the raw `err` object, which pino serializes as `{type,message,stack}`.
- Notable lines in this scope:
  - `🔒 Enabling mTLS with strict client certificate verification` (info, with `caPath` and `certPath`).
  - `🚀 gRPC Route Agent actively listening at h2://<HOST>:<port> (Proto Contract SHA256: <hash>)` (info, with `{protoHash, len}`) (`index.ts:191`).
  - `💥 Route Agent failed to start and is exiting; …` (fatal).
  - `Unauthorized <RPC> request blocked` / `… attempt rejected` (warn, no context).
  - `Starting agent self-update sequence...` (info); `Failed to execute self-update sequence` (error, `{err: stderr||message}`).
  - `Managed certificate not found or unreadable (issuance may still be in progress)` (warn, `{err, domain}`).
  - `Provisioned/updated AWG systemd unit file` (info, `{path}`); `Executed systemctl daemon-reload after updating AWG systemd unit file` (info).
- **Nothing in the orchestrator or agent consumes the agent's own journal.**
  - `PullNodeWarnings` reads only `sing-box`, the rear unit, `caddy` and the kernel (`src/utils/journalWarnings.ts:218-224`).
  - `StreamTelemetry` tails `journalctl -u sing-box`.
  - **Exact log text is therefore not a contract.** Keeping JSON-to-stdout with the same levels is SHOULD-level parity, useful for operators.

---

## 6. `exec.ts` (`src/utils/exec.ts:1-5`)

```ts
export const execAsync = promisify(exec);          // shell: /bin/sh -c "<string>"
export const execFileAsync = promisify(execFile);  // no shell, argv array
```
Node 22 semantics callers depend on:
- **`execAsync(cmd, opts?)`:**
  - Spawns `/bin/sh -c cmd`. On Debian and Ubuntu that is **dash**, not bash, which matters for `<(...)` (risk R-6).
  - Defaults: `encoding 'utf8'`, **`timeout 0` (none)**, **`maxBuffer 1024*1024` bytes per stream**. Exceeding it kills the child with SIGTERM and rejects with `ERR_CHILD_PROCESS_STDIO_MAXBUFFER`.
  - Env is inherited, and cwd is the process cwd (`/opt/route-agent`).
  - Resolves `{stdout, stderr}` (strings).
  - Rejects on non-zero exit or signal with an `Error` carrying `.code` (the exit code), `.signal`, `.stdout`, `.stderr` and `.cmd`. Callers read `err.stderr || err.message` (e.g. `system.service.ts:117`).
- **`execFileAsync(file, args, opts?)`:** same promise shape, no shell, `ENOENT` when the binary is missing (`err.code === 'ENOENT'`).
- **Usage:** 90 `execAsync` and 18 `execFileAsync` call sites. About 50 `execAsync` sites interpolate variables into the shell string (full list: `grep -rn 'execAsync(\`[^\`]*\${' src`). See risk R-1.
- Per-call `timeout` / `maxBuffer` overrides exist in some places, for example `meshTunnel.service.ts:132-251` and `awgFlows.ts:252`.
- **Go guidance:**
  - Provide `Sh(ctx, cmd)`, which runs `/bin/sh -c` with combined capture and preserves exit code, stdout and stderr, and `Exec(ctx, name, args...)`.
  - To be byte-compatible for shell commands that are themselves contract (`RELOAD_COMMAND`, `CADDY_RELOAD_COMMAND`, `SINGBOX_RESTART_COMMAND` are user-configurable shell strings), they **must still run via `/bin/sh -c`**.
  - Emulating the 1 MiB `maxBuffer` kill is not required (and is arguably a bug), but no-timeout-by-default is the current behaviour.

---

## 7. RPC `GetAgentInfo`

**Proto:** `rpc GetAgentInfo (AgentInfoRequest) returns (AgentInfoResponse)` (`proto/agent.proto:110`).
- `AgentInfoRequest { string orchestrator_secret = 1; }`
- `AgentInfoResponse { string proto_contract_hash = 1; string agent_version = 2; string proto_contract_source = 3; }`

### 7.1 Inputs and auth
- **There is no authentication, deliberately** (`system.service.ts:51-61`): it is a read-only pre-flight used before mutating calls, including for desynchronized nodes.
- `orchestrator_secret` is **ignored**, as is the metadata. The orchestrator still sends both (`orch:…/GrpcConfigPusher.ts:259`).
- Test: called with an empty `Metadata()` and it succeeds (`tests/app.test.ts:1292-1324`).

### 7.2 Processing
1. `protoContractHash = computeProtoContractHash()`, which uses the in-memory cache (§9.1).
2. `agentVersion = await getAgentVersion()` (`system.service.ts:13-49`), cached in memory for the process lifetime:
   1. Run `execAsync('git describe --tags --always')` (shell, cwd = process cwd = `/opt/route-agent`, no timeout). If stdout trimmed is non-empty, use it.
      - Formats: `v1.4.0` on a tag, `v1.4.0-3-gabc1234` after commits, or a bare abbreviated hash if there are no tags.
      - `--dirty` is **not** used.
   2. Otherwise read `<cwd>/package.json`, which yields `"0.0.0-development"` today (`package.json:3`).
   3. Otherwise `'1.0.0'`.
3. Response, status OK always:
   - `{protoContractHash, agentVersion, protoContractSource: 'canonical-json'}`. The snake twins are in the code but dropped on the wire.
   - On any exception (for example a proto file that can't be loaded), log error `Failed to process GetAgentInfo RPC` and return `{protoContractHash:'', agentVersion:'', protoContractSource:'canonical-json'}` (`:77-88`).
   - **No gRPC error is ever returned.**

### 7.3 Orchestrator dependency (MUST)

`orch:src/core/services/protoContractVerifier.ts:247-300`:
- It calls with a 10 s deadline and takes `info.protoContractHash || info.proto_contract_hash`.
- Classification:

  | Outcome | Status |
  |---|---|
  | `remoteHash === expectedHash` (non-empty) | `match` |
  | Otherwise (including an **empty** hash) | `mismatch`: **all mutating RPCs to the node are blocked**, except SelfUpdate (`orch:…/contractGuard.ts:37-52`) |
  | gRPC code 12 | `unimplemented` |
  | Any other error | `unreachable` |

- **`agentVersion` parsing** (`orch:src/core/services/agentCatchUp.ts:45-60`):
  - Regex `^v?(\d+\.\d+\.\d+)(?:-\d+-g[0-9a-f]+)?$`, which gives the base semver or `null`.
  - If a fleet target version is recorded and the base is `null` or lower than the target, the orchestrator **automatically fires SelfUpdate**, at most once per 6 h per node (`:34`, `:69-97`). Frozen fronts are skipped.
  - So a Go agent reporting a version in a different shape (for example `1.5.0+go`, `go-1.5.0`, `v1.5.0-dirty` or `0.0.0-development`) would be treated as "behind" and self-updated every 6 h.
  - The version is displayed in the node op journal as `· агент <version>` (`orch:src/core/services/nodeOps/actions.ts:367`).
- The verification runs on the first telemetry packet after each stream (re)connect, on the manual button, after a pending self-update, and when healing `unreachable`. It is deduplicated per node for 7 s (`protoContractVerifier.ts:126-241`).

### 7.4 The contract hash algorithm (MUST, bit-exact)

The orchestrator computes the same function over its own copy of `proto/agent.proto` (`orch:src/core/services/protoContractVerifier.ts:37-118`), so the two sides must produce identical bytes. For the current proto both produce **`21108c0ad83c24711d4dace9570420a14049ee541b29c6d7ffb250674c90ac9f`** (verified 2026-11-06 by running the algorithm on both files).

Algorithm (`src/services/protoContract.service.ts:13-106`):
1. Load the package definition with proto-loader, using the options in §3.1.
2. For each key of the package definition in JS default-sorted order (UTF-16 code-unit sort):
   - **Message** (`item.format === 'Protocol Buffer 3 DescriptorProto'` or `item.type.field` exists): `{ name: <fully qualified key, e.g. "agent.AgentInfoRequest">, fields: [...] }`.
     - Each field is `{ number, name, type, label, type_name }`:
       - `name` is the **camelCase name as produced by protobufjs** (`orchestrator_secret` → `orchestratorSecret`; protobufjs rule: `_` followed by `[a-z]` is removed and the letter uppercased);
       - `type` is the descriptor enum string (`TYPE_STRING`, `TYPE_UINT32`, `TYPE_MESSAGE`, …);
       - `label` is `LABEL_OPTIONAL` or `LABEL_REPEATED`, defaulting to `'LABEL_OPTIONAL'` when absent;
       - `type_name` is the **bare** message name for message fields (e.g. `"AwgPeer"`, no package, no leading dot), and the key is omitted entirely otherwise (JSON.stringify drops `undefined`).
     - Fields are sorted by `number` ascending.
   - **Service** (anything else that is an object): methods are the keys whose value has `.path`. Each method is `{ name, request_type: <bare request type name>, response_type, request_stream: bool, response_stream: bool }`, sorted with `localeCompare`. Only services with ≥1 method are kept, as `{ name: "agent.EgressAgentService", methods }`.
3. Sort `messages` and `services` by `name` with `localeCompare`.
4. `sortObjectKeys` recursively sorts **object keys** alphabetically. Arrays keep their order.
5. `JSON.stringify` with no whitespace, then SHA-256, then lowercase hex (64 chars).

Facts about the current proto that keep this tractable:
- 57 messages and 1 service with 31 methods.
- No enums, maps, oneofs, `optional` or nested types.
- No field name contains `_<digit>` or `_<Uppercase>`, which is where the protobufjs and protoc JSON-name rules diverge.
- For all current names, `localeCompare` order equals plain code-unit order (verified).

Example prefix of the canonical JSON: `{"messages":[{"fields":[{"label":"LABEL_OPTIONAL","name":"orchestratorSecret","number":1,"type":"TYPE_STRING"}],"name":"agent.AgentInfoRequest"},{"fields":[{"label":"LABEL_OPTIONAL","name":"protoContractHash","number":1,"type":"TYPE_STRING"},…`

The hash is invariant to declaration order in the `.proto` (test `tests/app.test.ts:1633-1656`) and changes with a field's type or number (`:1597-1631`). Field **name** changes also change it.

**Go guidance:**
- Generate the canonical JSON from the embedded `FileDescriptorProto` (protoreflect) with exactly these rules, **or** compute the hash at build time with the Node algorithm and embed it.
- Either way, add a golden test that compares against the orchestrator's `computeProtoContractHash` on the same `.proto`.
- Do not re-read `proto/agent.proto` from cwd at runtime, but if you keep that, preserve the "empty hash on failure" behaviour rather than crashing.

### 7.5 Logging and caching
- The hash is computed once at bind (for the startup log line) and cached in `cachedProtoHash`.
- The version is computed on the first `GetAgentInfo` and cached. A process restart (SelfUpdate) refreshes both.

---

## 8. RPC `SelfUpdate`

**Proto:** `rpc SelfUpdate (SelfUpdatePayload) returns (SelfUpdateResponse)` (`proto/agent.proto:37`).
- `SelfUpdatePayload {}` has **no fields**, so there is no body secret.
- `SelfUpdateResponse { bool success = 1; string message = 2; }`

### 8.1 Behaviour (`src/services/system.service.ts:94-120`)
1. Auth: **metadata only**. On failure: warn `Unauthorized SelfUpdate request blocked` and respond `{success:false, message:'Invalid orchestrator secret token.'}` (test `tests/app.test.ts:1268-1278`).
2. **Respond immediately** with `{success:true, message:'Self-update sequence initiated. Agent will pull code and restart in 1 second.'}`. The test asserts `success === true` and that the message includes `'Self-update sequence initiated'` (`tests/app.test.ts:1280-1290`).
3. `setTimeout(…, 1000)` (not unref'd). After 1 s:
   - log info `Starting agent self-update sequence...`;
   - `await execAsync('cd /opt/route-agent && git fetch --all --tags && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent')` (`:114`).
4. On rejection: log error `Failed to execute self-update sequence` with `{err: err.stderr || err.message}`. There is **no restart, no report to the orchestrator and no retry**.

Details that matter:
- **The path is hard-coded**, not derived from cwd or config.
- The shell is `/bin/sh -c`, and `&&` means the first failure stops the chain.
- No timeout, and a 1 MiB stdout/stderr `maxBuffer` (§6).
- Env is inherited:
  - systemd supplies `HOME=/root` and `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, since `User=root` sets HOME.
  - npm uses the global registry config that `install.sh` may have set to `registry.npmmirror.com` (`install.sh:225-228`). The comment there says the setting exists precisely because SelfUpdate runs `npm ci`.
- `git reset --hard @{u}` requires an upstream on the checked-out branch, which is the `main` from `git clone` by default.
- `git clean -fd` without `-x` keeps **gitignored** files: `node_modules/`, `dist/`, `.env`. It **deletes untracked, non-ignored** files, including `/opt/route-agent/active_ports.json` (risk R-4).
- `npm ci` installs devDependencies too, since `tsup` is needed for the build.
- `npm run build` runs `tsup` with `clean: true`, which **deletes `dist/` first** (`tsup.config.ts:8`) (risk R-3).
- `systemctl restart route-agent` kills the current process, the caller included (§1.4). The new process starts with a fresh version and hash cache.
- **There is no concurrency guard.** Two SelfUpdate calls within the window start two pipelines in parallel (risk R-5).

### 8.2 Orchestrator dependency (MUST)

`orch:…/GrpcConfigPusher.ts:758-794`; `orch:src/core/services/nodeOps/actions.ts:426-447`; `orch:…/protoContractVerifier.ts:302-391`:
- SelfUpdate is the **only** RPC that bypasses the contract guard (`skipContractGuard: true`), because it is the only way out of a `mismatch`. It is called with a 10-minute deadline, but only the immediate ack matters.
- The orchestrator registers a 180 s wait **before** sending the command. It treats "agent came back" as the **next telemetry stream (re)connect**, after which it re-runs `GetAgentInfo` and reports `match`/`mismatch` plus the version.
- Implication: the time from ack, through git, `npm ci`, the build and the restart, to the first telemetry packet MUST be under about 180 s. The stream only breaks at the final restart, and the orchestrator's reconnect ladder is 10 s, 30 s, then 5 min.
- Non-success ack text is surfaced as `обновление не запустилось: <message>` (`actions.ts:430-433`).
- Fleet update and the automatic catch-up (`orch:src/core/services/agentCatchUp.ts`) call the same RPC.

### 8.3 Migration constraint for the Go port (critical)

The **first** update of an existing box into the Go agent is executed by the **old TS agent**, with exactly this hard-coded pipeline and the existing unit (`ExecStart=<node> /opt/route-agent/dist/index.js`, `WorkingDirectory=/opt/route-agent`). Therefore the commit that `main` points to when boxes update MUST satisfy all of the following:
1. `npm ci` succeeds, so a valid `package.json` and `package-lock.json` must exist.
2. `npm run build` succeeds.
3. After `systemctl restart route-agent`, **`node /opt/route-agent/dist/index.js`** comes up, either as the agent itself or as a shim that installs or launches the Go binary and rewrites the unit.
4. It answers `GetAgentInfo` with the matching hash and a version string of the shape `vX.Y.Z[-N-gHASH]` within about 180 s.
5. `.env`, certificates and other state survive, which `git clean -fd` already guarantees for ignored files.

Without this, every box that self-updates crash-loops on `node dist/index.js` and becomes unmanageable: the orchestrator cannot SelfUpdate a dead agent, and recovery requires SSH. The Go agent's own SelfUpdate can choose a new mechanism, such as downloading a release binary, but the transition step is fixed by the TS code above.

---

## 9. Modules

### 9.1 `protoContract.service.ts`
- `PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto')` (`:5`).
- `cachedProtoHash` caches only for the default path (`:7`, `:84-106`). `clearProtoContractHashCache()` is test-only (`:111-113`).
- `sortObjectKeys`, `getCanonicalSchemaFromPackageDef` and `computeProtoContractHash` are specified in §7.4.
- The file is excluded from the snake/camel twin test because snake_case keys (`type_name`, `request_type`, …) are mandatory there (`tests/responseFieldNaming.test.ts:34`).
- `scripts/check-proto-sync.js` is a separate dev tool. It compares the **raw-text** SHA-256 of the local proto against `ORCHESTRATOR_PROTO_PATH` or `REMOTE_PROTO_URL`. It is not runtime.

### 9.2 `system.service.ts`
Covered in §7 (version detection and GetAgentInfo) and §8 (SelfUpdate).

### 9.3 `systemdUnit.service.ts`: `ensureAwgSystemdUnit(overridePath?)` (`:14-62`)
- `unitPath = overridePath || config.AWG_UNIT_FILE_PATH || '/etc/systemd/system/route-awg@.service'`.
- `awgBin = config.AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg'`; `awgQuickBin = awgBin + '-quick'`.
- **Expected content, byte-exact** (`:19-33`), with defaults substituted. It ends with a trailing newline:
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
- Steps:
  1. Read the existing file (utf-8). A read error counts as `null`.
  2. If it is **identical**, debug-log and return `false`: no write, no daemon-reload.
  3. Otherwise `mkdir -p dirname` and write the file. The mode is 0644 via umask, owner root, and the write is not atomic. Log info `Provisioned/updated AWG systemd unit file`.
  4. If `NODE_ENV !== 'test'`, run `execAsync('systemctl daemon-reload')`. A failure is a warn only. Return `true`.
  5. Any other error: log error `Failed to ensure AWG systemd unit file` and return `false`. It **never throws**.
- Tests (`tests/app.test.ts:536-556`): the first call returns `true` and the content contains the Description, `ExecStart=`, `ExecStop=`, `ExecReload=`, `<awg>-quick up %i` and `<awg> syncconf %i <(<awg>-quick strip %i)`; the second call returns `false`.
- It is the same template unit used for the client AWG (`route-awg@awg0`) and the mesh (`route-awg@awgmesh0`) (`src/config.ts:87-91`).
- **Go MUST write the identical bytes.** Any difference triggers a rewrite and `daemon-reload` on every boot, and changes the reload semantics (§12, R-6).

### 9.4 `ensureOlcrtcAgentSrvSystemdUnit(overridePath?)` (`src/services/olcrtc.service.ts:97-145`), called at boot
- It uses the same idempotent pattern. Content, byte-exact with defaults:
  ```
  [Unit]
  Description=olcrtc-agent-srv instance %i (managed by route-agent)
  After=network-online.target
  Wants=network-online.target

  [Service]
  Type=simple
  User=root
  ExecStart=/usr/local/bin/olcrtc-agent-srv /etc/olcrtc-agent-srv/%i.yaml
  Restart=always
  RestartSec=5

  [Install]
  WantedBy=multi-user.target
  ```
- Path `config.OLCRTC_AGENT_SRV_UNIT_FILE_PATH`. Log messages are the same shape as AWG's, with "olcrtc-agent-srv". Test `tests/app.test.ts:558-573`.

### 9.5 `certStorage.ts` (`src/utils/certStorage.ts:1-29`)
- `sanitizeDomainForStorage(domain)` (`:9-17`) applies these steps **in this order**:
  1. `' '` → `'_'` (global)
  2. `'+'` → `'_plus_'`
  3. `'*'` → `'wildcard_'`
  4. `':'` → `'-'`
  5. `'..'` → `''` (global, non-overlapping, left to right, single pass)
  6. `toLowerCase()`
  - There is **no trimming and no removal of other characters**, including `/`. This differs from certmagic's real `KeyBuilder.Safe`, which also trims and strips `[^\w@.-]` (risk R-10).
- `CADDY_CERT_BASE = '/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory'` (`:19-20`).
- `getCaddyCertPaths(domain, base=CADDY_CERT_BASE)` (`:22-29`) returns `{certPath: join(base, safe, safe + '.crt'), keyPath: join(base, safe, safe + '.key')}`.
  - Example: `*.front.example.com` → `…/wildcard_.front.example.com/wildcard_.front.example.com.crt`.
- Users:
  - `GetManagedCertificate` reads;
  - `ConfigureCaddy` writes `extra_cert_*` and `extra_certificates[]` into the same layout (`config.service.ts:320,348`), with key files mode `0o600` and certs at the default mode.
- The orchestrator's sing-box and Caddy generators build `certificate_path` and `key_path` with the same convention, so the layout MUST NOT change.

### 9.6 `plannedWrites.ts` (`src/utils/plannedWrites.ts:1-70`)
- `PlannedWrite { path: string; content: string; mode?: number }`.
- **`allWritesAlreadyOnDisk(writes)`** (`:38-52`):
  - An empty list returns **`false`** ("nothing to write" is not "verified").
  - For each write, in order: read utf-8 (a missing file is a mismatch), and return `false` if the string differs.
  - If `mode` is set, `stat`, and return `false` if `(st.mode & 0o777) !== mode` (`:45-48`).
  - Returns `true` only if everything matches.
  - Rationale: a correct key with drifted permissions must be rewritten and chmod'ed (`:28-37`). Incident 2026-09-04: two identical `ConfigureCaddy` calls in one second each reloaded Caddy (`:5-19`).
- **`applyPlannedWrites(writes)`** (`:55-70`):
  - Sequential. For each write: `mkdir -p dirname` (0755 via umask).
  - Without `mode`: `writeFile(utf-8)`.
  - With `mode`: `writeFile(…, {mode})`, which only applies on creation, then `chmod(path, mode)` with errors swallowed. The comment at `:66-67` explains why the chmod is a separate call.
  - **Not atomic**: truncate and write, no rename, no fsync. A failure mid-list leaves earlier files written and propagates the error.
- Used only by `ConfigureCaddy` (`config.service.ts:365,381`), where it is combined with an "is Caddy active" check: everything identical **and** Caddy active means skip the reload.
- Tests: `tests/plannedWrites.test.ts` cover:
  - a missing file is not a match;
  - one differing file is a mismatch;
  - an empty plan is not a match;
  - a 0644 file versus an expected 0600 is a mismatch;
  - chmod is applied on an existing file;
  - directories are created.

---

## 10. Background loops and boot helpers

| Loop | Started | Interval | First tick | Overlap guard | unref | Stops |
|---|---|---|---|---|---|---|
| AWG health and self-heal | `startServer` step 5, if `NODE_ENV !== 'test'` | 30 s (`setInterval`) | after 30 s | **none** | no | never (`stopAwgHealthCheckTimer` is unused in prod) |
| Rear WARP guard | after bind, non-test | `REAR_WARP_GUARD_INTERVAL_MS` (30 s) | after one interval | `running` flag (skips the tick) | yes | never |
| Kernel tuning | after bind, non-test | one-shot | immediately (async) | n/a | n/a | n/a |
| Per-stream telemetry tick (not boot) | on each `StreamTelemetry` | 2 s | | | | on stream end (see the telemetry spec) |

### 10.1 `sanitizeAwgToolsSymlink` (`src/services/binary.service.ts:282-297`)
- `lstat(AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg')`. If it is a **symlink**: warn `Self-heal: removed legacy symlink at AWG_TOOLS_BINARY_PATH created by deprecated UploadAwgBinary RPC` with `{path, target}`, then `unlink`. All errors are ignored.
- The legacy RPC symlinked `awg` to `amneziawg-go` (test `tests/app.test.ts:510-534`).

### 10.2 `fixXraySocketPermissions` (`src/utils/caddy.ts:153-196`)
- Paths are hard-coded: `/dev/shm/vless-xhttp.sock` and `/dev/shm/vless-grpc.sock`. They must match the orchestrator's Caddyfile generator defaults (`:146-152`).
- For each path: `stat`, and skip silently if absent. Otherwise `execAsync('chmod 666 <path>')`. A failure is a warn `Failed to adjust permissions on Xray unix socket` and processing continues with the next path. It never throws.
- Why 666: the sockets are created by Remnawave's xray (an unknown user), and Caddy (user `caddy`) must connect. There is no shared group (`:158-176`).
- Test `tests/app.test.ts:1942-1989`:
  - exactly one `chmod 666 <existing>` call;
  - none for missing paths;
  - continues after a failure.

### 10.3 AWG health timer (`src/utils/telemetry.ts:476-614`)

This is summarized here because the timer is started at boot; telemetry's use of it belongs to the telemetry spec.
- `checkAndHealAwgInterface(iface = basename(AWG_CONFIG_PATH,'.conf') || 'awg0')`.
- **Configured** = `stat(AWG_CONFIG_PATH)` exists **or** `systemctl is-enabled route-awg@<iface>` stdout is `enabled`. If not configured: status `inactive`, done.
- **Alive** = `ip link show <iface>` exits 0.
- If not alive and (never restarted, or ≥5 min since the last auto-restart): `systemctl restart route-awg@<iface>`. On success, set `lastAutoRestartTime` and increment `autoRestartCount`. On failure, log error. Otherwise warn about the cooldown with `waitSec`.
- Status strings, consumed by telemetry `awgStatus`:
  - `nominal`;
  - `nominal (restarted Nm ago)` / `nominal (restarted Nh ago)` if a restart happened within the last 24 h;
  - `crashed`;
  - `crashed (restarted …)`.
  - Minutes are shown if under 60, else hours.
- State is in-memory per interface. Tick errors are logged at debug, so they are invisible.
- `getAwgStatus()` (telemetry) also calls `checkAndHealAwgInterface`, so healing also runs on telemetry demand.

### 10.4 Rear WARP guard (`src/services/rearWarpGuard.service.ts:30-95`, logic in `src/utils/rearWarpGuard.ts`)

The details belong to the rear spec. Per tick:
1. `readActiveRearClashApi()`: work out which rear core's config exists on disk, parse it, and get the Clash API `address` and `secret`.
2. `GET http://<addr>/proxies` with `authorization: Bearer <secret>` if a secret is set, and a 5 s timeout. A non-2xx response throws.
3. Decide: if the `warp` selector is absent, do nothing. Otherwise the desired member is pool (`wg-pool`, or `warp-auto` for mihomo) if any key is alive, else direct (`direct`, or `DIRECT` for mihomo).
4. If the current `now` differs, `PUT /proxies/warp` with body `{"name":"<member>"}` and a 5 s timeout, then log warn `Rear WARP branch fell back to direct: no live keys left` or `Rear WARP branch restored: live keys are back`.

Errors are logged at debug only. It lives on the agent deliberately, so that it works while the orchestrator is down (`src/index.ts:194-200`).

### 10.5 Kernel tuning (`src/utils/kernelTuning.ts:79-194`), run on every start (non-test)

1. `writeSysctlFile()`: if `SYSCTL_PROFILE_PATH` content differs from the rendering, `mkdir -p` and write it.
   - **This step is not wrapped in try/catch.** An error rejects `applyKernelTuning`, which is called as `void applyKernelTuning()`, so the result is an **unhandled rejection and the process crashes** (risk R-11).
2. For each entry, in order: `execFile('sysctl', ['-w', 'key=value'])`.
   - On failure: warn `Kernel tuning: sysctl key could not be set` and record `applied:false`, then continue with the next key.
   - On success: `execFile('sysctl', ['-n', key])`, collapse whitespace, and record it as `effective`. A failed read still counts as applied.
3. Log info `Kernel tuning: network buffer profile applied` with `{fileChanged, applied, failed}`.

Exact file content (MUST be byte-identical, or the file is rewritten on every boot):
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
(The file ends with a single `\n`.) The rationale is that every key was measured (`:9-71`):
- `rmem_default` is the real lever;
- `rmem_max` is not doubled;
- `netdev_max_backlog`, `ip_local_port_range`, `udp_mem`, `tcp_rmem` and `tcp_wmem` are deliberately excluded (`tests/kernelTuning.test.ts:230-245`).

There is no RPC and no screen for this, by design (`src/index.ts:202-213`).

---

## 11. RPC `GetManagedCertificate`

**Proto:** `rpc GetManagedCertificate (CertificateRequest) returns (CertificateResponse)` (`proto/agent.proto:116`).
- `CertificateRequest { string orchestrator_secret = 1; string domain = 2; }`
- `CertificateResponse { bool success = 1; string message = 2; string fullchain_pem = 3; string private_key_pem = 4; }`

### 11.1 Behaviour (`src/services/config.service.ts:455-487`)
1. Auth M|B. On failure: warn `Unauthorized GetManagedCertificate request blocked` and respond `{success:false, message:'Invalid orchestrator secret token.'}`.
2. Validation: if `domain` is falsy, not a string, or `domain.trim()` is empty, respond `{success:false, message:'Missing domain parameter.'}`.
   - The **untrimmed** `domain` is used for the path.
3. `getCaddyCertPaths(domain)` (§9.5). Read `certPath` and `keyPath` **in parallel** as utf-8.
4. Success: `{success:true, message:'OK', fullchainPem, privateKeyPem}`. It returns the **private key** over the wire, by design: the orchestrator relays it to egress nodes.
5. Any read error:
   - warn `Managed certificate not found or unreadable (issuance may still be in progress)` with `{err, domain}`;
   - respond `{success:false, message: err.message}`, the raw Node fs message, e.g. `ENOENT: no such file or directory, open '/var/lib/caddy/…/wildcard_.x.example.com.crt'`.
6. It never returns a gRPC error. Status is OK on every path.

It is read-only, with no locks and no caching.

### 11.2 Orchestrator dependency

The call goes through `executeAgentRpc` (`orch:…/GrpcConfigPusher.ts:323-338`):
- the contract guard applies (blocked on `mismatch`);
- the default 60 s deadline;
- `onError:'returnFailure'`, `errorStyle:'grpcCode'`;
- success revives telemetry and heals `unreachable`.

Callers and what they read:

| Caller | Domain | Reads |
|---|---|---|
| `orch:src/core/services/XeonService.ts:509-517` (fetch the front wildcard) | `*.${controlDomain}` | Needs `success && fullchainPem && privateKeyPem`. Otherwise shows `certRes.message` to the admin, falling back to Russian text. |
| `orch:src/core/services/frontCerts/renewal.ts:46-50` | `*.${controlDomain}` | `success && fullchainPem`; the PEM is parsed for `notAfter` |
| `orch:src/core/services/frontCerts/verify.ts:69-70` (egress nodes) | the ring domain | `success && fullchainPem`; the PEM fingerprint is compared with the DB |

No caller string-matches the message. `fullchainPem` and `privateKeyPem` MUST be the file contents verbatim. The fingerprint and `notAfter` are computed from the PEM, so trailing newlines and chain order must not be altered.

---

## 12. `install.sh` and `uninstall.sh`: what exists on a box

### 12.1 How install is invoked

The orchestrator generates a wrapper script (`orch:src/core/generators/installScript.ts:41-105`):
- It writes `/tmp/route-agent-install-inner.sh` (mode 600), which runs `curl -fsSL "<installerUrl>?v=$(date +%s)" | bash -s -- --secret "<bundle>" --port <grpcPort> [--domain "<controlDomain>" --decoy-port <port>]` with `DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 NEEDRESTART_MODE=l`.
- It runs that inner script inside `tmux` session `route-agent-install` (falling back to `setsid --fork`), with output tee'd to `/var/log/route-agent-install.log` (mode 600).
- The installer URL defaults to `https://raw.githubusercontent.com/Ametas/route-agent/main/install.sh` and can be overridden by the `node_installer_url` setting (`orch:src/core/services/orchestrator/systemOrchestrator.ts:573-581`).
- **The Go-era install.sh MUST keep the same CLI**: `--secret`, `--port`, `--repo`, `--domain`, `--decoy-port`. An unknown flag means `exit 1` (`install.sh:87-97`).

**Secret bundle** (`orch:src/shared/utils/certs.ts:305-317`): `base64(JSON.stringify({ca, cert, key, secret}))`, where:
- `ca` is the CA cert PEM;
- `cert` is the node cert PEM;
- `key` is the node key PEM (PKCS#8);
- `secret` is 64 hex chars.

A non-bundle `--secret` is treated as the raw secret (legacy path, no certs written).

### 12.2 Steps, in order (`install.sh`, `set -e`)
1. Require root (`EUID`), else `exit 1` (`:6-9`). Export `DEBIAN_FRONTEND=noninteractive`, `NEEDRESTART_SUSPEND=1` and `NEEDRESTART_MODE=l`. Reason (`:15-23`): needrestart killed the installer's own SSH session four times in a row.
2. Define helpers:
   - `fetch()` is `curl -fsSL --connect-timeout 10 --max-time 180`.
   - `reachable()` is `curl -fsS -o /dev/null --connect-timeout 8 --max-time 15`.
   - Mirrors are `cdn.npmmirror.com/binaries/node` and `registry.npmmirror.com`. Reason (`:25-32`): a Moscow VPS couldn't reach Cloudflare-fronted NodeSource or npm.
3. Parse arguments.
   - Defaults: `PORT=8081`, `REPO=https://github.com/Ametas/route-agent.git`, `AGENT_DIR=/opt/route-agent`, `DECOY_PORT=8443`.
   - `--secret` is required (`:99-102`).
   - `--domain` and `--decoy-port` are only echoed at the end (`:301-305`). They have no other effect.
4. `apt-get update`, then `apt-get install -y iptables iproute2 ufw git curl unzip xz-utils mtr-tiny conntrack debian-keyring debian-archive-keyring apt-transport-https ca-certificates gnupg` (`:106-107`).
5. If `caddy` is not installed:
   - If Cloudsmith is reachable: add its key to `/usr/share/keyrings/caddy-stable-archive-keyring.gpg` (temp file, then `gpg --dearmor --yes`), add `/etc/apt/sources.list.d/caddy-stable.list`, and run `apt-get update`.
   - Otherwise remove that list.
   - Then `apt-get install -y caddy` (`:109-122`).
6. UFW: `ufw allow 22/tcp`, `443/tcp`, `443/udp` and `$PORT/tcp`, then `ufw --force enable`. Each command ends with `|| true` (`:125-129`).
7. Write `/etc/sudoers.d/route-agent-ufw` with the content `ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw` and mode `0440` (`:131-134`) (risk R-2).
8. `mkdir -p /etc/sing-box /var/www/decoy /var/lib/caddy /etc/amnezia/amneziawg`; `chmod 755 /var/lib/caddy`; **overwrite** `/etc/sing-box/config.json` with `{"route":{"rules":[]}}` (`:140-142`) (risk R-7).
9. Node.js 22, if `node` **or** `npm` is missing.
   - Reason (`:145-148`): a bare `node` without `npm` (for example from remnanode) once broke `npm ci`.
   - Prefer the NodeSource apt repository (`/etc/apt/keyrings/nodesource.gpg`, `/etc/apt/sources.list.d/nodesource.list`).
   - Otherwise download from the mirror: `latest-v22.x`, sha256-checked against `SHASUMS256.txt` from the same mirror, extracted into `/usr/local` (`:56-78`, `:149-164`).
   - If `npm` is still missing: `exit 1` with a PATH-shadowing hint (`:166-169`).
10. Swap: if no active swap and no fstab swap entry, create `/swapfile` (2G via `fallocate`, or `dd` as a fallback), `chmod 600`, `mkswap`, `swapon`, and append `/swapfile none swap sw 0 0` to `/etc/fstab`. A failure only warns (`:176-197`).
11. Code:
    - If `/opt/route-agent` is missing: `git clone $REPO`.
    - If it is a git repository: `git fetch --all --tags && git reset --hard @{u} && git clean -fd`, the same idiom as SelfUpdate (`:199-215`).
    - If it exists but is not a git repository: `exit 1`.
12. If `registry.npmjs.org` is unreachable: `npm config set registry https://registry.npmmirror.com --location=global`. This is persisted globally for future SelfUpdates (`:222-228`).
13. `npm ci`, then `npm run build` (`:230-231`).
14. Certificates: `mkdir -p /etc/route-agent/certs`. A `node -e` script decodes the base64 JSON. If it has `secret`, `ca`, `cert` and `key`:
    - write `ca.crt`, `agent.crt` and `agent.key` (default mode 0644; `agent.key` then `chmod 0600`);
    - print the secret to `/tmp/route-agent-secret.tmp`, from which `SECRET` is read before the file is removed (`:233-262`).
    - The secret is passed as an argv entry to `node -e`, so it is briefly visible in `ps`.
15. Write `/opt/route-agent/.env` (§2.2) and `chmod 600` (`:264-274`).
16. Write `/etc/systemd/system/route-agent.service` (§1.1), then `systemctl daemon-reload`, `systemctl enable route-agent` and `systemctl restart route-agent` (`:277-299`).

### 12.3 On-box artifact inventory the Go agent must find and keep honouring

| Path | Created by | Mode/owner | Meaning |
|---|---|---|---|
| `/opt/route-agent/` (git checkout of `main`) | install | root | WorkingDirectory. Holds `.git` (version), `proto/agent.proto` (read at runtime by TS), `package.json` (version fallback) and `dist/index.js` (ExecStart). |
| `/opt/route-agent/.env` | install | 0600 root | Environment (§2.2). Kept across `git clean -fd` (gitignored). |
| `/opt/route-agent/active_ports.json` | agent (firewall) | 0644 | UFW UDP port cache (`src/utils/firewall.ts:12`). **Deleted by `git clean -fd`** (R-4). |
| `/etc/route-agent/certs/ca.crt`, `agent.crt` | install | 0644 root | mTLS |
| `/etc/route-agent/certs/agent.key` | install | **0600** root | mTLS |
| `/etc/route-agent/{rear.json, rear.yaml, rules/, traffic-throttle.json}` | agent at runtime | various | rear and throttle state (other specs) |
| `/etc/systemd/system/route-agent.service` | install | 0644 | §1.1 |
| `/etc/systemd/system/route-awg@.service` | agent at boot | 0644 | §9.3 |
| `/etc/systemd/system/olcrtc-agent-srv@.service` | agent at boot | 0644 | §9.4 |
| `/etc/sysctl.d/99-route-agent.conf` | agent at boot | 0644 | §10.5 |
| `/etc/sudoers.d/route-agent-ufw` | install | 0440 | Needed because agent code calls `sudo ufw …` (e.g. `src/services/firewall.service.ts:39,47`) even though it runs as root |
| `/etc/sing-box/config.json`, `/var/www/decoy`, `/var/lib/caddy` (0755), `/etc/amnezia/amneziawg` | install | | Base layout |
| `/swapfile` + `/etc/fstab` entry | install (conditional) | 0600 | |
| npm global `registry` | install (conditional) | | Used by SelfUpdate's `npm ci` |
| `/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/<safe>/<safe>.{crt,key}` | Caddy ACME (fronts) / ConfigureCaddy (egress) | crt default, key 0600 | §9.5, §11 |

### 12.4 `uninstall.sh` (CLI: `--port`, default 8081; unknown flag means exit 1)

Steps in order:
1. Stop and disable these units, each with `|| true`:
   - `route-agent`, `sing-box`, `route-rear-singbox`, `route-rear-mihomo`;
   - every `olcrtc-agent-srv@*` unit (from `systemctl list-units --type=service --all --no-legend`);
   - `caddy` and `awg-quick@awg0`.
2. UFW, if installed: `ufw delete allow 443/tcp`, `443/udp`, `$PORT`, `$PORT/tcp` and `$PORT/udp`, then `ufw reload`.
3. `rm -f /etc/sudoers.d/route-agent-ufw`.
4. Remove the unit files `route-agent.service`, `sing-box.service`, `olcrtc-agent-srv@.service`, `route-rear-singbox.service` and `route-rear-mihomo.service`.
5. Remove `/etc/route-agent/rear.json`, `/etc/sing-box/rear.json` (pre-2026-09-04 location, holds WARP private keys), `/etc/route-agent/rear.yaml`, `/etc/route-agent/rules` and `/etc/sysctl.d/99-route-agent.conf`.
6. `systemctl daemon-reload`.
7. `rm -rf /opt/route-agent /etc/route-agent /etc/sing-box /etc/caddy /etc/amnezia/amneziawg /etc/olcrtc-agent-srv /var/www/decoy`, then `rm -f /usr/local/bin/{sing-box,mihomo,olcrtc-agent-srv} /tmp/sing-box.download`.
8. If `caddy` is installed: `apt-get purge -y caddy`, remove the caddy apt list and keyring, and run `apt-get autoremove -y`.

Gaps are listed under R-13.

---

## 13. Non-obvious "why" details

These are the details most likely to be lost in a rewrite:
- **No insecure fallback.** Silent degradation would leak `x-orchestrator-secret` in plaintext (`src/index.ts:64-74`). The orchestrator applies the same fail-closed rule on its side (`orch:…/GrpcConnectionManager.ts:209-223`).
- **Keepalive timeout of 20 s, mirrored on both ends.** 5 s caused `read ECONNRESET` and "node unreachable" on GC pauses or packet loss (`src/index.ts:128-140`, 2026-09-18).
- **`CADDY_RELOAD_COMMAND` default includes `|| systemctl restart caddy`.** A reload never re-reads `EnvironmentFile=`, so a Caddy started before `cloudflare.env` existed stays blind to `CF_API_TOKEN`. An `||` fallback in code would be dead because Zod defaults only apply when the variable is absent. In production, 5 reload failures never triggered a restart (`src/config.ts:59-67`; `tests/app.test.ts:1876-1889`).
- **`SINGBOX_RESTART_COMMAND` is separate from `RELOAD_COMMAND`.** SIGHUP keeps the old image; incident `aeza-msc-1`, 2026-09-20: `unknown inbound type: users-api` (`src/config.ts:14-24`).
- **The rear config lives outside `/etc/sing-box`** because of `-C /etc/sing-box` directory merging (`src/config.ts:26-34`).
- **Mihomo paths are separate** so that `sing-box check` over every sing-box config doesn't choke on mihomo's (`src/config.ts:38-41`).
- **`plannedWrites` compares content and permissions over the whole set:**
  - two identical ConfigureCaddy calls per second each reloaded Caddy;
  - comparing only the Caddyfile would turn a redundant reload into a silent non-delivery of a changed cert;
  - permissions must count, or a key with drifted permissions is "fresh" forever (`src/utils/plannedWrites.ts:5-37`).
- **`chmod` after `writeFile`**, because `mode` only applies on creation (`:66-67`).
- **`GetAgentInfo` is unauthenticated** as a pre-flight that works even for desynchronized nodes (`src/services/system.service.ts:51-61`).
- **The version comes from `git describe --tags`** so the orchestrator can compare "newer or not" (2026-09-25; `src/services/system.service.ts:16-23`).
- **SelfUpdate bypasses the contract guard on the orchestrator side**, the only way out of a mismatch (`orch:…/GrpcConfigPusher.ts:758-775`).
- **`install.sh` re-syncs an existing checkout** with the same git idiom as SelfUpdate, because "fix not applied after re-run" was a real incident (`install.sh:199-205`).
- **npm mirror in the global config** so SelfUpdate's `npm ci` uses it too (`install.sh:222-224`).
- **Swapfile**, because OOM during `npm ci` or the build killed sshd instead (`install.sh:171-175`).
- **needrestart suspension**, because it restarted ssh mid-install (`install.sh:15-23`; `orch:src/core/generators/installScript.ts:12-40`).
- **Kernel tuning on every start**, not at install. This lets the profile reach the fleet via SelfUpdate and re-assert after reboots. Each key is applied separately, because some keys are read-only in OpenVZ or containers (`src/index.ts:202-213`, `src/utils/kernelTuning.ts:135-145`).
- **The WARP guard lives on the agent** because WARP failure during orchestrator downtime is exactly when the fallback is needed (`src/index.ts:194-200`, `src/utils/rearWarpGuard.ts:13-16`).
- **Xray socket permissions are `chmod 666`, not a group,** because xray's user is unknown and uncontrolled (`src/utils/caddy.ts:158-176`).
- **`certStorage` mirrors certmagic's on-disk layout** so the orchestrator's cert path construction needs no special case (`src/utils/certStorage.ts:3-8`).
- **Uploads are rejected and the stream destroyed** so the sender stops pushing bytes (`src/services/binaryReceiver.ts:51-55`).
- **The temp upload is removed before the response,** so "done" means `/tmp` is clean (`src/services/binaryReceiver.ts:127-130`).
- **Snake/camel twin rule.** proto-loader silently drops non-camel keys; this was nearly shipped broken in ConfigureRearSingbox (`tests/responseFieldNaming.test.ts:11-31`).

---

## 14. Parity checklist (testable)

**Process and config**
- [ ] With `CA_CERT_PATH`, `AGENT_CERT_PATH` and `AGENT_KEY_PATH` all present and valid, the agent listens on `HOST:PORT` (default `0.0.0.0:8081`) with TLS and **requires** a client cert chained to `ca.crt`.
- [ ] If any of the three files is missing: exit code 1, an error containing `mTLS certificates are required but unavailable`, `MISSING at <path>` and `refuses to start`; no listener is opened.
- [ ] If the files exist but can't be read (key path is a directory): exit 1 with `mTLS certificates were found but failed to load` and `refuses to start`.
- [ ] Missing `EGRESS_CONTROL_SECRET`: exit 1 before listening. An empty secret starts, but every authenticated RPC fails auth.
- [ ] Every default in §2.2 is identical when the variable is absent. Unknown variables are ignored. The exact install-generated `.env` (§2.2) parses, including the unquoted value with spaces.
- [ ] `CADDY_RELOAD_COMMAND` default is exactly `systemctl reload caddy || systemctl restart caddy`.
- [ ] Env from the process wins over `.env` in cwd (dotenv no-override semantics).

**Transport**
- [ ] A client cert from a different CA fails the handshake.
- [ ] A CA-signed cert with only `serverAuth` EKU is rejected as a client cert.
- [ ] Server PINGs every 10 s; the connection is dropped after 20 s without an ack.
- [ ] A client pinging every 10 s with no active calls (orchestrator settings) is **never** GOAWAY'd for too many pings, including with no streams for minutes.
- [ ] Unary requests up to 4 MiB are accepted. Larger ones get `RESOURCE_EXHAUSTED`.
- [ ] The presented server certificate's leaf DER SHA-256 equals that of `agent.crt` (pin compatibility); SNI = node UUID is accepted.
- [ ] Unknown method returns `UNIMPLEMENTED (12)`.

**Auth**
- [ ] Every RPC in §4.3 rejects a wrong or missing secret with exactly the listed response shape and `message:'Invalid orchestrator secret token.'`.
- [ ] Server-streaming RPCs reject with status `UNAUTHENTICATED (16)` and send no data.
- [ ] RPCs marked M\|B accept the secret either in metadata `x-orchestrator-secret` or in the body `orchestrator_secret`. RPCs marked M **ignore** the body secret, so metadata is required.
- [ ] Client-streaming uploads accept the secret in metadata, or in the first chunk. A first chunk without a valid secret, with no valid metadata, means rejection, the temp file is removed, and the client sees `success:false`.
- [ ] Secret comparison is constant-time over equal lengths, and an empty expected secret never matches.

**GetAgentInfo**
- [ ] Works with no metadata and no body secret.
- [ ] `proto_contract_hash` is 64 lowercase hex and equals the orchestrator's `computeProtoContractHash` for the same proto. Today that is `21108c0ad83c24711d4dace9570420a14049ee541b29c6d7ffb250674c90ac9f`.
- [ ] `proto_contract_source` = `canonical-json`.
- [ ] `agent_version` matches `^v?\d+\.\d+\.\d+(-\d+-g[0-9a-f]+)?$` for a released build (same shape as `git describe --tags --always`), is non-empty, and differs from the hash.
- [ ] Internal failure returns status OK with empty strings for hash and version, never a gRPC error.
- [ ] Hash is invariant to declaration order in the `.proto` and changes when any field number, type, name or label changes.

**SelfUpdate**
- [ ] Bad metadata secret: `{success:false, message:'Invalid orchestrator secret token.'}`; nothing is executed.
- [ ] Good secret: immediate `{success:true, message:'Self-update sequence initiated. Agent will pull code and restart in 1 second.'}`, then the update starts about 1 s later, asynchronously.
- [ ] After the update the agent restarts, and the new process reports the new version and hash. The whole cycle fits in under 180 s on a typical box.
- [ ] A failed update leaves the running agent serving, with an error logged.
- [ ] The TS→Go transition step works via the **old** TS pipeline (§8.3).

**GetManagedCertificate**
- [ ] Bad secret gives the auth failure shape; a body secret alone is accepted.
- [ ] Empty or whitespace-only `domain`: `{success:false, message:'Missing domain parameter.'}`.
- [ ] `*.x.example.com` reads `/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/wildcard_.x.example.com/wildcard_.x.example.com.{crt,key}` and returns both verbatim with `success:true, message:'OK'`.
- [ ] A missing file returns `success:false` with a message naming the error, status OK.
- [ ] The sanitization table (§9.5) is reproduced exactly, including `'..'` removal and lowercasing and no trimming. Golden cases: `'A B'`→`a_b`; `'x+y'`→`x_plus_y`; `'*.Ex.com'`→`wildcard_.ex.com`; `'h:443'`→`h-443`; `'a..b'`→`ab`; `'....'`→`''`; `'...'`→`'.'`.

**Shared helpers**
- [ ] `allWritesAlreadyOnDisk([])` is false; it is true only when every file's content matches and (when `mode` is set) `perm & 0777 == mode`.
- [ ] `applyPlannedWrites` creates parent directories and enforces `mode` on pre-existing files.
- [ ] `ensureAwgSystemdUnit` and `ensureOlcrtcAgentSrvSystemdUnit` write the exact bytes in §9.3 and §9.4. A second call writes nothing, returns false and does not daemon-reload. A changed file triggers `systemctl daemon-reload`, and its failure is non-fatal.
- [ ] Boot removes a symlink at `AWG_TOOLS_BINARY_PATH`, leaving regular files alone.
- [ ] Boot runs `chmod 666` on each existing `/dev/shm/vless-{xhttp,grpc}.sock` only and never fails.
- [ ] Boot writes the sysctl profile only if it differs, with byte-exact content (§10.5); applies each key separately; one failing key doesn't stop the others.
- [ ] AWG health tick every 30 s: restarts `route-awg@<iface>` at most once per 5 minutes when it is configured but the link is down; status strings exactly as in §10.3.
- [ ] Rear WARP guard every `REAR_WARP_GUARD_INTERVAL_MS`, with no overlapping passes.

**Install**
- [ ] `install.sh` keeps its flags; accepts both the base64 bundle and a raw secret; produces the same paths, modes and unit as §12.3; re-running on an installed box resyncs the code and keeps the certificates.

---

## 15. Risks and smells (flagged, not fixed)

- **R-1. Shell injection surface.** About 50 `execAsync` calls interpolate values into `/bin/sh -c` strings. Examples:
  - `sudo ufw allow ${port}/udp` (`src/services/firewall.service.ts:39,47`; `src/utils/firewall.ts:49,161,173`);
  - `dkms … -m ${name} -v ${version}` with values parsed from an uploaded tarball's `dkms.conf` (`src/services/meshTunnel.service.ts:132-241`);
  - `tar -xzf "${tarPath}" … -C "${buildDir}"`;
  - `${caddyBin} validate --config "${caddyfilePath}"` (`config.service.ts:400`);
  - `systemctl … ${unitName}` with unit names built from request data (olcrtc `user_short_uuid`, validated there);
  - `setcap … ${tempPath}`.

  Callers are authenticated (mTLS + secret), so this is defence in depth. **The Go port should use argv exec everywhere it can**; it must keep `sh -c` only for the user-configurable command variables.
- **R-2. `/etc/sudoers.d/route-agent-ufw` grants `ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw`.** Any local user gets passwordless root ufw, which can reconfigure or disable the firewall. It is unnecessary because the agent runs as root.
- **R-3. A failed self-update can brick the next start.**
  - tsup `clean: true` wipes `dist/` before building. A failed build leaves no `dist/index.js`.
  - The pipeline then skips the restart, so the old process keeps running in memory.
  - At the next restart (a reboot, a crash, or the next SelfUpdate) the agent crash-loops on a missing `dist/index.js`, and the orchestrator can no longer reach it.
  - `git reset --hard` has also already moved the tree.
- **R-4. `git clean -fd` deletes `/opt/route-agent/active_ports.json`** (not gitignored) on every SelfUpdate and install re-run, so the firewall port cache is silently lost (`src/utils/firewall.ts:12`).
- **R-5. No dedupe or lock on SelfUpdate.**
  - Concurrent calls run concurrent `git`, `npm ci` and build pipelines in the same tree: `index.lock` conflicts and interleaved `node_modules`.
  - Each successful one restarts the agent.
  - There is no timeout, so a hung `git fetch` or `npm ci` holds the pipeline forever.
  - The 1 MiB `maxBuffer` can kill a verbose `npm ci` mid-way.
- **R-6. `route-awg@.service` `ExecReload` uses bash process substitution `<(...)` under `/bin/sh -c`.** On Debian and Ubuntu `/bin/sh` is dash, so the line is a syntax error and every `systemctl reload route-awg@…` fails. Callers fall back to `restart` (`config.service.ts:654-659`, `meshTunnel.service.ts:524-529`). Effective behaviour today is therefore "reload = restart". The Go port must keep the unit bytes identical, or deliberately fix it in both places.
- **R-7. `install.sh` overwrites `/etc/sing-box/config.json`** with an empty skeleton on **every** run, including a re-run on a live node. The node then serves no sing-box inbounds until the next ApplyConfig push.
- **R-8. `ufw --force enable` with only `22/tcp` allowed** locks out boxes whose SSH listens on another port.
- **R-9. Secret exposure on the box:**
  - `EGRESS_CONTROL_SECRET` is inherited by every child process (npm lifecycle scripts during SelfUpdate, `caddy validate`, …);
  - the bundle (including the private key) appears in `node -e` argv during install;
  - `/tmp/route-agent-secret.tmp` is created with the default umask (0644) for a moment;
  - the secret is also in tmux and log output only if echoed (it isn't).
- **R-10. `sanitizeDomainForStorage` diverges from certmagic** (no trim, no stripping of `[^\w@.-]`, `/` kept).
  - `'..'` removal prevents escaping the base directory, but a domain containing `/` maps to nested paths inside the base.
  - The ACME directory is hard-coded to Let's Encrypt production. If a front's Caddy falls back to ZeroSSL, or uses the LE staging CA, the cert lands under a different directory and `GetManagedCertificate` reports ENOENT forever.
- **R-11. `void applyKernelTuning()` turns a sysctl file write error into an unhandled rejection and a process crash.** Examples: a read-only `/etc` or an immutable file. On such a box the agent crash-loops every 5 s although the code comments say tuning failure must only warn (`src/utils/kernelTuning.ts:183-194` is not inside try/catch).
- **R-12. Empty numeric env.** `z.coerce.number()` turns an empty value into 0:
  - `PORT=` means a random port;
  - `REAR_WARP_GUARD_INTERVAL_MS=` means a 1 ms loop hammering the Clash API.
  - There are no range checks.
- **R-13. `uninstall.sh` gaps:**
  - It stops `awg-quick@awg0`, but the agent uses `route-awg@awg0`. `route-awg@awg0` and `route-awg@awgmesh0` stay running and enabled, and `/etc/systemd/system/route-awg@.service` is never removed.
  - It leaves `/usr/local/bin/{awg,awg-quick,amneziawg-go,caddy-custom}`, the Caddy override drop-in `/etc/systemd/system/caddy.service.d/override.conf`, `/swapfile`, the npm registry setting, the NodeSource apt list and the DKMS amneziawg module.
- **R-14. Dead or ignored config:**
  - `AWG_RELOAD_COMMAND` is never read.
  - `AWG_QUICK_BINARY_PATH` is not used by the unit, which derives `${AWG_TOOLS_BINARY_PATH}-quick`.
  - `.env.example` documents `OLCRTC_BINARY_PATH` and `OLCRTC_MANAGER_BINARY_PATH`, which no longer exist.
  - README documents removed RPCs (`UploadOlcrtcBinary`, `ConfigureOlcrtc`).
- **R-15. The proto is read from cwd at runtime.** A wrong `WorkingDirectory` crashes at import, outside the fatal handler. If the proto is unreadable later, `GetAgentInfo` returns an empty hash, which the orchestrator treats as `mismatch`: every mutating RPC to the node is blocked.
- **R-16. The version fallback `0.0.0-development`** (from `package.json` when `git` fails or `.git` is absent) parses as `null` on the orchestrator. With a fleet target recorded, that causes an automatic SelfUpdate every 6 h.
- **R-17. No graceful shutdown and no global error handlers.** Any thrown error in an async handler outside its try/catch kills the agent (Node 22 default).
- **R-18. The AWG health timer has no overlap guard and no exec timeouts.** A hung `systemctl restart route-awg@awg0` stacks ticks every 30 s.
- **R-19. Upload auth rejection is racy.** `callback(success:false)` followed by `call.destroy()` lets clients see either a response or an error.
- **R-20. Installer integrity.** The installer comes from a mutable branch URL and is piped into `bash` as root without pinning or a checksum. The orchestrator backlog already records this (`orch:.claude/BACKLOG.md:270-275`).

---

## 16. Could not determine

- Whether any production box carries an **RSA** node certificate or key issued before the ECDSA switch. The orchestrator's git history in this checkout is too shallow. The Go agent should simply accept any PEM key type.
- Whether `/bin/sh` on every fleet box is dash. This decides whether R-6 is universal. It is assumed from Debian/Ubuntu defaults.
- Whether any box has a manually set `CADDY_BINARY_PATH` or other non-install variables in `/opt/route-agent/.env`. Nothing in either codebase writes them.
- The exact client-visible outcome of the upload auth rejection (R-19). It depends on grpc-js internals, and the tests accept both outcomes.
- The intended Go deployment mechanism after the transition (binary download versus build on the box). §8.3 only fixes the transition constraint.
