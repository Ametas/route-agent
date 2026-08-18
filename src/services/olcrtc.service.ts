import { ServerReadableStream, ServerUnaryCall, ServerWritableStream, sendUnaryData } from '@grpc/grpc-js';
import * as fs from 'fs/promises';
import { createWriteStream } from 'fs';
import { spawn } from 'child_process';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata, authenticateCall } from '../middleware/auth.js';
import { replaceBinaryAtomically } from './binary.service.js';

const logger = pino({ level: 'info' });

// --- OLCRTC (план `olcrtc-redesign.md`, "Новая архитектура: собственный слой") ------------------
// route-agent runs olcrtc-agent-srv (our own thin wrapper around openlibrecommunity/olcrtc's
// pkg/olcrtc/tunnel, see that repo's README) directly — one systemd unit INSTANCE per (user, node)
// pair, olcrtc-agent-srv@<user_short_uuid>.service, config at
// <OLCRTC_AGENT_SRV_CONFIG_DIR>/<user_short_uuid>.yaml. No third-party admin daemon, no HTTP admin
// port, no Basic Auth — replaces the old ConfigureOlcrtc/UploadOlcrtcBinary (proxied Olcrtc_manager).

function sanitizeConfigInput(val: string | number | undefined | null): string {
  if (val === undefined || val === null) return '';
  return String(val).replace(/[\r\n]/g, '').trim();
}

/** user_short_uuid becomes a systemd instance name (%i) — keep it to the safe subset systemd
 * instance names and a bare filename actually tolerate, so a malformed/hostile value can't inject
 * a path segment or a systemd specifier. Remnawave's own shortUuid values are alphanumeric+dashes
 * in practice, so this is not expected to ever reject a real one. */
function isSafeInstanceName(value: string): boolean {
  return /^[A-Za-z0-9_-]{1,128}$/.test(value);
}

function instanceConfigPath(userShortUuid: string): string {
  return path.join(config.OLCRTC_AGENT_SRV_CONFIG_DIR, `${userShortUuid}.yaml`);
}

function instanceUnitName(userShortUuid: string): string {
  return `olcrtc-agent-srv@${userShortUuid}`;
}

/**
 * YAML field names mirror olcrtc-agent-srv's own config.go schema exactly (mode/auth/room/crypto/
 * net/vp8/sei) — hand-templated rather than pulling in a YAML library, same style as this
 * codebase's other generated config files (AWG's INI-style .conf). All inputs pass through
 * sanitizeConfigInput first (strips CR/LF — the one character class that could break out of a
 * single YAML scalar line written this way).
 */
function buildInstanceYaml(payload: any): string {
  const carrier = sanitizeConfigInput(payload.carrier);
  const transport = sanitizeConfigInput(payload.transport);
  const roomId = sanitizeConfigInput(payload.roomId ?? payload.room_id);
  const keyHex = sanitizeConfigInput(payload.cryptoKeyHex ?? payload.crypto_key_hex);
  const dns = sanitizeConfigInput(payload.dns) || '8.8.8.8:53';

  let yaml = 'mode: srv\n';
  yaml += 'auth:\n';
  yaml += `  provider: ${carrier}\n`;
  yaml += 'room:\n';
  yaml += `  id: "${roomId}"\n`;
  yaml += 'crypto:\n';
  yaml += `  key: "${keyHex}"\n`;
  yaml += 'net:\n';
  yaml += `  transport: ${transport}\n`;
  yaml += `  dns: "${dns}"\n`;

  const vp8Fps = payload.vp8Fps ?? payload.vp8_fps;
  const vp8Batch = payload.vp8BatchSize ?? payload.vp8_batch_size;
  if (transport === 'vp8channel' && (vp8Fps || vp8Batch)) {
    yaml += 'vp8:\n';
    if (vp8Fps) yaml += `  fps: ${Number(vp8Fps) | 0}\n`;
    if (vp8Batch) yaml += `  batch_size: ${Number(vp8Batch) | 0}\n`;
  }

  const seiFps = payload.seiFps ?? payload.sei_fps;
  const seiBatch = payload.seiBatchSize ?? payload.sei_batch_size;
  const seiFrag = payload.seiFragmentSize ?? payload.sei_fragment_size;
  const seiAck = payload.seiAckTimeoutMs ?? payload.sei_ack_timeout_ms;
  if (transport === 'seichannel' && (seiFps || seiBatch || seiFrag || seiAck)) {
    yaml += 'sei:\n';
    if (seiFps) yaml += `  fps: ${Number(seiFps) | 0}\n`;
    if (seiBatch) yaml += `  batch_size: ${Number(seiBatch) | 0}\n`;
    if (seiFrag) yaml += `  fragment_size: ${Number(seiFrag) | 0}\n`;
    if (seiAck) yaml += `  ack_timeout_ms: ${Number(seiAck) | 0}\n`;
  }

  return yaml;
}

/**
 * Идемпотентно создаёт/обновляет templated systemd unit-файл olcrtc-agent-srv@.service — тот же
 * паттерн, что ensureAwgSystemdUnit (route-awg@.service): пишет только при реальном изменении,
 * daemon-reload только тогда же. `%i` — user_short_uuid этого инстанса, конфиг лежит по тому же
 * имени в OLCRTC_AGENT_SRV_CONFIG_DIR.
 */
export async function ensureOlcrtcAgentSrvSystemdUnit(overridePath?: string): Promise<boolean> {
  const unitPath = overridePath || config.OLCRTC_AGENT_SRV_UNIT_FILE_PATH;
  const binPath = config.OLCRTC_AGENT_SRV_BINARY_PATH;
  const configDir = config.OLCRTC_AGENT_SRV_CONFIG_DIR;

  const expectedContent = `[Unit]
Description=olcrtc-agent-srv instance %i (managed by route-agent)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=${binPath} ${configDir}/%i.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`;

  try {
    const existingContent = await fs.readFile(unitPath, 'utf-8').catch(() => null);

    if (existingContent === expectedContent) {
      logger.debug({ path: unitPath }, 'olcrtc-agent-srv systemd unit file is already up to date; skipping write and daemon-reload');
      return false;
    }

    await fs.mkdir(path.dirname(unitPath), { recursive: true });
    await fs.writeFile(unitPath, expectedContent, 'utf-8');
    logger.info({ path: unitPath }, 'Provisioned/updated olcrtc-agent-srv systemd unit file');

    if (process.env.NODE_ENV !== 'test') {
      try {
        await execAsync('systemctl daemon-reload');
        logger.info('Executed systemctl daemon-reload after updating olcrtc-agent-srv systemd unit file');
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to execute systemctl daemon-reload');
      }
    }

    return true;
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg, path: unitPath }, 'Failed to ensure olcrtc-agent-srv systemd unit file');
    return false;
  }
}

/**
 * RPC Обработчик SyncOlcrtcInstance — пишет YAML-конфиг инстанса и (пере)запускает его systemd-
 * юнит. Идемпотентно: одинаковый YAML не пропускает restart зря, только когда контент реально
 * изменился (то же правило, что для самого unit-файла).
 */
export async function syncOlcrtcInstanceHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized SyncOlcrtcInstance request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const userShortUuid = sanitizeConfigInput(call.request.userShortUuid ?? call.request.user_short_uuid);
  if (!isSafeInstanceName(userShortUuid)) {
    logger.warn({ userShortUuid }, 'Rejected SyncOlcrtcInstance with unsafe/empty user_short_uuid');
    return callback(null, { success: false, message: 'Invalid or missing user_short_uuid.' });
  }

  try {
    await ensureOlcrtcAgentSrvSystemdUnit();

    const configPath = instanceConfigPath(userShortUuid);
    const yamlContent = buildInstanceYaml(call.request);

    const existingContent = await fs.readFile(configPath, 'utf-8').catch(() => null);
    const contentChanged = existingContent !== yamlContent;

    await fs.mkdir(path.dirname(configPath), { recursive: true });
    await fs.writeFile(configPath, yamlContent, 'utf-8');

    const unitName = instanceUnitName(userShortUuid);

    if (process.env.NODE_ENV !== 'test') {
      try {
        await execAsync(`systemctl enable ${unitName}`);
      } catch (err: any) {
        logger.warn({ err: err.message, unitName }, 'Failed to enable olcrtc-agent-srv instance for auto-start');
      }

      if (contentChanged) {
        try {
          await execAsync(`systemctl restart ${unitName}`);
        } catch (err: any) {
          const msg = (err.stderr || err.stdout || err.message || 'systemctl restart error').trim();
          logger.error({ err: msg, unitName }, 'Failed to (re)start olcrtc-agent-srv instance');
          return callback(null, { success: false, message: `Failed to start olcrtc instance (${unitName}): ${msg}` });
        }
      } else {
        // Config unchanged — just make sure it's actually running (e.g. after a crash-loop or a
        // previous partial failure), same idempotent-no-op-if-already-correct spirit as the rest
        // of this codebase's Sync-shaped RPCs.
        try {
          await execAsync(`systemctl start ${unitName}`);
        } catch (err: any) {
          logger.debug({ err: err.message, unitName }, 'systemctl start no-op (likely already running)');
        }
      }
    }

    return callback(null, {
      success: true,
      message: `olcrtc instance ${userShortUuid} configured (${contentChanged ? 'restarted' : 'unchanged'}).`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg, userShortUuid }, 'Failed to sync olcrtc instance');
    return callback(null, { success: false, message: `Olcrtc instance sync error: ${msg}` });
  }
}

/**
 * RPC Обработчик DeleteOlcrtcInstance — stop+disable юнита, удаление YAML-конфига. Идемпотентно:
 * повторный вызов на уже удалённом инстансе — не ошибка (`|| true` на systemctl-командах, отсутствие
 * файла конфига молча игнорируется), тот же принцип, что остальные Delete/disable-пути в проекте.
 */
export async function deleteOlcrtcInstanceHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized DeleteOlcrtcInstance request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const userShortUuid = sanitizeConfigInput(call.request.userShortUuid ?? call.request.user_short_uuid);
  if (!isSafeInstanceName(userShortUuid)) {
    logger.warn({ userShortUuid }, 'Rejected DeleteOlcrtcInstance with unsafe/empty user_short_uuid');
    return callback(null, { success: false, message: 'Invalid or missing user_short_uuid.' });
  }

  const unitName = instanceUnitName(userShortUuid);

  try {
    if (process.env.NODE_ENV !== 'test') {
      try {
        await execAsync(`systemctl disable --now ${unitName} || true`);
      } catch (err: any) {
        logger.warn({ err: err.message, unitName }, 'Failed to stop/disable olcrtc instance (continuing with cleanup)');
      }
    }

    await fs.unlink(instanceConfigPath(userShortUuid)).catch(() => {});

    return callback(null, { success: true, message: `olcrtc instance ${userShortUuid} removed.` });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg, userShortUuid }, 'Failed to delete olcrtc instance');
    return callback(null, { success: false, message: `Olcrtc instance delete error: ${msg}` });
  }
}

/**
 * RPC Обработчик StreamOlcrtcEvents — server-streaming, один открытый стрим на ноду форвардит
 * события session_open/session_close/traffic/health со ВСЕХ активных инстансов на этой ноде в
 * реальном времени. Источник — собственные stdout JSON-строки olcrtc-agent-srv (см. тот репо,
 * events.go), захваченные journald per-instance-юнита; `journalctl -u 'olcrtc-agent-srv@*' -f
 * --output json` даёт и саму строку (MESSAGE), и то, какой именно юнит её написал (_SYSTEMD_UNIT)
 * — из имени юнита восстанавливаем user_short_uuid, не полагаясь на сам JSON-пейлоад (тот ничего
 * не знает о том, к какому пользователю относится — это чисто systemd-уровневая метаинформация).
 */
export async function streamOlcrtcEventsHandler(
  call: ServerWritableStream<any, any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized StreamOlcrtcEvents request blocked');
    const err = new Error('UNAUTHENTICATED: Invalid orchestrator secret token.') as any;
    err.code = 16; // grpc.status.UNAUTHENTICATED
    call.emit('error', err);
    return;
  }

  logger.info('OLCRTC events stream opened by orchestrator');

  const journalProcess = spawn('journalctl', ['-u', 'olcrtc-agent-srv@*', '-n', '0', '-f', '--output', 'json']);
  let isCleanedUp = false;
  let lineBuffer = '';

  const cleanup = () => {
    if (isCleanedUp) return;
    isCleanedUp = true;
    if (journalProcess && !journalProcess.killed && journalProcess.pid) {
      try {
        journalProcess.kill('SIGTERM');
      } catch {}
    }
    logger.info('OLCRTC events stream closed and resources safely released');
  };

  call.on('cancelled', cleanup);
  call.on('close', cleanup);
  call.on('finish', cleanup);
  call.on('error', cleanup);

  journalProcess.on('error', (err: any) => {
    logger.warn({ err: err.message }, 'Failed to spawn journalctl process for OLCRTC events');
  });

  const unitPattern = /^olcrtc-agent-srv@(.+)\.service$/;

  if (journalProcess.stdout) {
    journalProcess.stdout.on('error', (err: any) => {
      logger.debug({ err: err.message }, 'Journalctl stdout stream error ignored (OLCRTC events)');
    });
    journalProcess.stdout.on('data', (chunk: Buffer) => {
      if (isCleanedUp || call.destroyed) return;
      lineBuffer += chunk.toString();

      let newlineIndex: number;
      while ((newlineIndex = lineBuffer.indexOf('\n')) >= 0) {
        const line = lineBuffer.slice(0, newlineIndex);
        lineBuffer = lineBuffer.slice(newlineIndex + 1);
        if (!line.trim()) continue;

        try {
          const journalEntry = JSON.parse(line);
          const unitName = String(journalEntry._SYSTEMD_UNIT || '');
          const match = unitPattern.exec(unitName);
          if (!match) continue;
          const userShortUuid = match[1];

          const message = journalEntry.MESSAGE;
          if (!message || typeof message !== 'string') continue;

          const event = JSON.parse(message);
          if (isCleanedUp || call.destroyed) return;

          call.write({
            userShortUuid,
            type: event.type || '',
            timestamp: event.ts ? Date.parse(event.ts) || Date.now() : Date.now(),
            sessionId: event.session_id || '',
            deviceId: event.device_id || '',
            reason: event.reason || '',
            addr: event.addr || '',
            bytesIn: event.bytes_in || 0,
            bytesOut: event.bytes_out || 0,
            healthJson: event.health ? JSON.stringify(event.health) : '',
          });
        } catch {
          // Malformed/unrelated journal line (e.g. our own stderr diagnostic lines, which aren't
          // JSON) — skip it, never let one bad line take down the whole stream.
        }
      }
    });
  }
}

/**
 * RPC Обработчик UploadOlcrtcAgentSrvBinary (клиентский стрим RPC) — заменяет старый
 * UploadOlcrtcBinary (грузил стороннего olcrtc-manager'а). Единственный бинарь, без ALLOWED_BINARIES-
 * ветвления, что было нужно только для различения olcrtc/olcrtc-manager в старом дизайне.
 */
export async function uploadOlcrtcAgentSrvBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/olcrtc-agent-srv_${uniqueId}.tmp`;
  let targetVersion = 'unknown';
  let bytesWritten = 0;
  let fileStream: ReturnType<typeof createWriteStream> | null = null;

  call.on('data', (data: any) => {
    if (isAborted) return;

    if (!secretVerified) {
      if (verifySecret(data?.orchestratorSecret || data?.orchestrator_secret)) {
        secretVerified = true;
      }
    }

    if (!secretVerified) {
      isAborted = true;
      logger.warn('Unauthorized UploadOlcrtcAgentSrvBinary attempt rejected');
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (data.version) {
      targetVersion = data.version;
    }
    if (data.chunk && data.chunk.length > 0) {
      if (!fileStream) {
        fileStream = createWriteStream(tempPath);
      }
      const buf = Buffer.from(data.chunk);
      fileStream.write(buf);
      bytesWritten += buf.length;
    }
  });

  call.on('end', async () => {
    if (isAborted) return;

    if (fileStream) {
      await new Promise<void>((resolve) => fileStream!.end(resolve));
    }

    if (!secretVerified) {
      isAborted = true;
      await fs.unlink(tempPath).catch(() => {});
      logger.warn('Unauthorized UploadOlcrtcAgentSrvBinary attempt rejected');
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (bytesWritten === 0) {
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    try {
      const targetPath = config.OLCRTC_AGENT_SRV_BINARY_PATH;

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      await replaceBinaryAtomically(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});

      logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated olcrtc-agent-srv binary');

      const unitJustProvisioned = await ensureOlcrtcAgentSrvSystemdUnit();

      if (process.env.NODE_ENV !== 'test' && !unitJustProvisioned) {
        // Unit already existed — a binary upgrade should restart every currently-running instance
        // so they pick up the new binary (each instance is `Type=simple`, `ExecStart` re-executes
        // the (now-updated) target path fresh on restart).
        try {
          const { stdout } = await execAsync(
            "systemctl list-units --type=service --state=running --no-legend 'olcrtc-agent-srv@*' | awk '{print $1}'"
          );
          const runningUnits = stdout.split('\n').map((l) => l.trim()).filter(Boolean);
          for (const unit of runningUnits) {
            try {
              await execAsync(`systemctl restart ${unit}`);
            } catch (err: any) {
              logger.warn({ err: err.message, unit }, 'Failed to restart olcrtc instance after binary upgrade');
            }
          }
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to enumerate running olcrtc-agent-srv instances after binary upgrade');
        }
      }

      return callback(null, {
        success: true,
        message: `olcrtc-agent-srv binary version ${targetVersion} successfully updated`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded olcrtc-agent-srv binary');
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadOlcrtcAgentSrvBinary stream');
    cleanup();
  });
  call.on('cancelled', () => {
    cleanup();
  });
}
