import { ServerReadableStream, ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import * as fs from 'fs/promises';
import { createWriteStream } from 'fs';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync, execFileAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';
import { invalidateSingboxVersionCache, invalidateAwgVersionCache } from '../utils/telemetry.js';
import { restartActiveAwgServices } from '../utils/awg.js';
import { ensureSingboxSystemdUnit } from '../utils/singbox.js';
import { ensureCaddyCustomBinaryOverride } from '../utils/caddy.js';

const logger = pino({ level: 'info' });

/**
 * Выполняет атомарную замену бинарного файла через промежуточный временный файл в той же директории назначения.
 * Это предотвращает ошибки EXDEV (между разными файловыми системами) и ETXTBSY (при перезаписи запущенного исполняемого файла).
 */
export async function replaceBinaryAtomically(sourcePath: string, targetPath: string): Promise<void> {
  const targetDir = path.dirname(targetPath);
  await fs.mkdir(targetDir, { recursive: true });

  const tempTarget = path.join(
    targetDir,
    `.${path.basename(targetPath)}_${Date.now()}_${crypto.randomUUID().slice(0, 6)}.tmp`
  );

  try {
    // Copy/move source into a temp file located IN THE SAME DESTINATION DIRECTORY
    await fs.copyFile(sourcePath, tempTarget);
    await fs.chmod(tempTarget, 0o755);

    // Atomic rename within the same directory (same filesystem) never throws EXDEV,
    // and Linux kernel permits atomic rename over a currently running executable.
    await fs.rename(tempTarget, targetPath);
  } finally {
    await fs.unlink(tempTarget).catch(() => {});
  }
}

/**
 * RPC Обработчик UploadSingboxBinary (клиентский стрим RPC)
 */
export async function uploadSingboxBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/sing-box_${uniqueId}.tmp`;
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
      logger.warn('Unauthorized UploadSingboxBinary attempt rejected');
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
      logger.warn('Unauthorized UploadSingboxBinary attempt rejected');
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
      const targetPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      if (process.env.NODE_ENV !== 'test') {
        await execFileAsync(tempPath, ['version']);

        if (process.platform === 'linux') {
          try {
            await execAsync(`setcap 'cap_net_admin,cap_net_bind_service=+ep' ${tempPath}`);
          } catch (err: any) {
            logger.warn({ err: err.message }, 'Failed to setcap on new sing-box binary');
          }
        }
      }

      await replaceBinaryAtomically(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});

      if (process.env.NODE_ENV !== 'test' && process.platform === 'linux') {
        try {
          await execAsync(`setcap 'cap_net_admin,cap_net_bind_service=+ep' ${targetPath}`);
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to setcap on target sing-box binary');
        }
      }

      logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated sing-box binary');
      invalidateSingboxVersionCache();

      // Юнит sing-box больше не создаётся заранее в install.sh — провижинится здесь, лениво,
      // в момент первой (или любой последующей) реальной загрузки бинарника, зеркально
      // паттерну AWG/Olcrtc. Делаем это ДО любой попытки systemctl start/reload для него.
      const unitJustProvisioned = await ensureSingboxSystemdUnit();

      if (process.env.NODE_ENV !== 'test') {
        if (unitJustProvisioned) {
          // Первый реальный вызов: юнит только что создан — поднимаем демон сразу с
          // доставленным бинарником и placeholder-конфигом и переживаем перезагрузки.
          try {
            const { stdout, stderr } = await execAsync('systemctl enable --now sing-box');
            if (stdout) logger.info({ stdout }, 'sing-box enabled and started after unit provisioning');
            if (stderr) logger.warn({ stderr }, 'sing-box enable --now stderr');
          } catch (err: any) {
            logger.warn({ err: err.message }, 'Failed to enable/start sing-box after provisioning its systemd unit');
          }
        } else {
          // Юнит уже актуален — sing-box уже управляется штатным ApplyConfig-путём (reload).
          try {
            const reloadCmd = config.RELOAD_COMMAND || 'systemctl restart sing-box';
            const { stdout, stderr } = await execAsync(reloadCmd);
            if (stdout) logger.info({ stdout }, 'Restart/Reload after binary upgrade');
            if (stderr) logger.warn({ stderr }, 'Restart/Reload stderr');
          } catch (err: any) {
            logger.warn({ err: err.message }, 'Reload command failed after binary upgrade');
          }
        }
      }

      return callback(null, {
        success: true,
        message: `sing-box binary version ${targetVersion} successfully updated and restarted`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded sing-box binary');
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadSingboxBinary stream');
    cleanup();
  });
  call.on('cancelled', () => {
    cleanup();
  });
}

/**
 * RPC Обработчик UploadOlcrtcBinary (универсальный клиентский стрим RPC для olcrtc / olcrtc-manager)
 */
export async function uploadOlcrtcBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  let targetVersion = 'unknown';
  let targetBinary = 'olcrtc-manager';
  let bytesWritten = 0;

  const ALLOWED_BINARIES = new Set(['olcrtc', 'olcrtc-manager']);
  let fileStream: ReturnType<typeof createWriteStream> | null = null;
  let tempPath = '';

  call.on('data', (data: any) => {
    if (isAborted) return;

    if (!secretVerified) {
      if (verifySecret(data?.orchestratorSecret || data?.orchestrator_secret)) {
        secretVerified = true;
      }
    }

    if (!secretVerified) {
      isAborted = true;
      logger.warn('Unauthorized UploadOlcrtcBinary attempt rejected');
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (data.version) {
      targetVersion = data.version;
    }
    if (data.targetBinary || data.target_binary) {
      targetBinary = (data.targetBinary || data.target_binary)!;
    }
    if (data.chunk && data.chunk.length > 0) {
      if (!fileStream) {
        const safeTargetBinary = ALLOWED_BINARIES.has(targetBinary) ? targetBinary : 'olcrtc-manager';
        const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
        tempPath = `/tmp/${safeTargetBinary}_${uniqueId}.tmp`;
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
      if (tempPath) await fs.unlink(tempPath).catch(() => {});
      logger.warn('Unauthorized UploadOlcrtcBinary attempt rejected');
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (bytesWritten === 0) {
      if (tempPath) await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    try {
      const safeTargetBinary = ALLOWED_BINARIES.has(targetBinary) ? targetBinary : 'olcrtc-manager';
      let targetPath = (safeTargetBinary === 'olcrtc') 
        ? (config.OLCRTC_BINARY_PATH || '/usr/local/bin/olcrtc')
        : (config.OLCRTC_MANAGER_BINARY_PATH || '/usr/local/bin/olcrtc-manager');

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      await replaceBinaryAtomically(tempPath, targetPath);
      if (tempPath) await fs.unlink(tempPath).catch(() => {});

      logger.info({ path: targetPath, binary: targetBinary, version: targetVersion }, 'Atomically updated olcrtc component binary');

      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync('systemctl restart olcrtc || true');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to restart olcrtc service after binary upload');
        }
      }

      return callback(null, {
        success: true,
        message: `${targetBinary} binary successfully updated to version ${targetVersion}`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded olcrtc binary');
      if (tempPath) await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    if (tempPath) fs.unlink(tempPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadOlcrtcBinary stream');
    cleanup();
  });
  call.on('cancelled', () => {
    cleanup();
  });
}

/**
 * Defensive health check при старте ноды (self-heal для очистки legacy-состояния):
 * Если AWG_TOOLS_BINARY_PATH является символической ссылкой, удаляет её и логирует warning.
 */
export async function sanitizeAwgToolsSymlink(): Promise<void> {
  const awgToolsPath = config.AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg';
  try {
    const stat = await fs.lstat(awgToolsPath);
    if (stat.isSymbolicLink()) {
      const linkTarget = await fs.readlink(awgToolsPath).catch(() => 'unknown');
      logger.warn(
        { path: awgToolsPath, target: linkTarget },
        'Self-heal: removed legacy symlink at AWG_TOOLS_BINARY_PATH created by deprecated UploadAwgBinary RPC'
      );
      await fs.unlink(awgToolsPath);
    }
  } catch {
    // Игнорируем ошибки, если файл/симлинк не существует
  }
}

/**
 * RPC Обработчик UpgradeSingbox
 */
export async function upgradeSingboxHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized UpgradeSingbox request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { version, downloadUrl, download_url } = call.request;
  const url = downloadUrl || download_url;
  const targetVersion = version || 'latest';

  if (!url) {
    return callback(null, { success: false, message: 'Missing download_url in request payload.' });
  }

  try {
    const parsedUrl = new URL(url);
    if (parsedUrl.protocol !== 'http:' && parsedUrl.protocol !== 'https:') {
      return callback(null, { success: false, message: 'Invalid URL protocol: must be http or https.' });
    }
  } catch {
    return callback(null, { success: false, message: 'Invalid download_url format.' });
  }

  const targetPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/sing-box_${uniqueId}.tmp`;

  try {
    logger.info({ version: targetVersion, url }, 'Initiating sing-box binary upgrade via download URL...');

    await execFileAsync('curl', ['-fsSL', '--connect-timeout', '10', '--max-time', '300', '--proto', '=http,https', '--', url, '-o', tempPath]);
    await fs.chmod(tempPath, 0o755);

    if (process.env.NODE_ENV !== 'test') {
      await execFileAsync(tempPath, ['version']);
      if (process.platform === 'linux') {
        try {
          await execAsync(`setcap 'cap_net_admin,cap_net_bind_service=+ep' ${tempPath}`);
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to setcap on downloaded sing-box binary');
        }
      }
    }

    await replaceBinaryAtomically(tempPath, targetPath);
    await fs.unlink(tempPath).catch(() => {});

    if (process.env.NODE_ENV !== 'test' && process.platform === 'linux') {
      try {
        await execAsync(`setcap 'cap_net_admin,cap_net_bind_service=+ep' ${targetPath}`);
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to setcap on target sing-box binary');
      }
    }

    invalidateSingboxVersionCache();
    logger.info({ path: targetPath, version: targetVersion }, 'Successfully upgraded sing-box binary via URL');

    if (process.env.NODE_ENV !== 'test') {
      try {
        const reloadCmd = config.RELOAD_COMMAND || 'systemctl restart sing-box';
        const { stdout, stderr } = await execAsync(reloadCmd);
        if (stdout) logger.info({ stdout }, 'Restart/Reload after binary upgrade');
        if (stderr) logger.warn({ stderr }, 'Restart/Reload stderr');
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Reload command failed after binary upgrade');
      }
    }

    return callback(null, {
      success: true,
      message: `sing-box binary version ${targetVersion} successfully upgraded from ${url}`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to upgrade sing-box binary via URL');
    await fs.unlink(tempPath).catch(() => {});
    return callback(null, { success: false, message: `Failed to upgrade sing-box binary: ${msg}` });
  }
}

/**
 * RPC Обработчик UploadAwgToolsBinary (клиентский стрим RPC для загрузки бинарника amneziawg-tools / awg)
 */
export async function uploadAwgToolsBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/awg_tools_${uniqueId}.tmp`;
  let targetVersion = 'unknown';
  let targetBinaryName = '';
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
      logger.warn('Unauthorized UploadAwgToolsBinary attempt rejected');
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (data.targetBinary || data.target_binary) {
      targetBinaryName = data.targetBinary || data.target_binary;
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

    if (!secretVerified || bytesWritten === 0) {
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No valid binary data received or unauthorized.' });
    }

    try {
      const isAwgQuick = targetBinaryName === 'awg-quick';
      const targetPath = isAwgQuick
        ? (config.AWG_QUICK_BINARY_PATH || '/usr/local/bin/awg-quick')
        : (config.AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg');

      await fs.mkdir(path.dirname(tempPath), { recursive: true });

      if (isAwgQuick) {
        // Проверка заголовка shebang (#!) для скрипта awg-quick
        const contentHeader = await fs.readFile(tempPath, { encoding: 'utf-8', flag: 'r' }).catch(() => '');
        if (!contentHeader.startsWith('#!')) {
          await fs.unlink(tempPath).catch(() => {});
          logger.warn({ path: tempPath }, 'Uploaded awg-quick script missing shebang (#!) header');
          return callback(null, {
            success: false,
            message: 'Invalid awg-quick script: missing shebang (#!) header.'
          });
        }

        // Проверка синтаксиса bash -n на платформах Linux/Unix
        if (process.platform !== 'win32') {
          try {
            await execAsync(`bash -n ${tempPath}`);
          } catch (err: any) {
            const errMsg = (err.stderr || err.stdout || err.message || '').trim();
            logger.error({ err: errMsg }, 'awg-quick script syntax validation failed via bash -n');
            await fs.unlink(tempPath).catch(() => {});
            return callback(null, {
              success: false,
              message: `Invalid awg-quick script syntax: ${errMsg}`
            });
          }
        }
      }

      await fs.chmod(tempPath, 0o755);
      await replaceBinaryAtomically(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});

      if (isAwgQuick) {
        logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated awg-quick script');
      } else {
        logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated awg-tools binary');
      }
      invalidateAwgVersionCache();

      let serviceMsg = '';
      if (process.env.NODE_ENV !== 'test') {
        const restartRes = await restartActiveAwgServices();
        if (restartRes.reloaded) {
          serviceMsg = ' and active AWG service(s) restarted';
        } else {
          serviceMsg = ' (service restart skipped: not yet configured)';
        }
      }

      const label = isAwgQuick ? 'awg-quick script' : 'awg-tools (awg) binary';
      return callback(null, {
        success: true,
        message: `${label} version ${targetVersion} successfully updated${serviceMsg}`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded awg-tools binary');
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadAwgToolsBinary stream');
    cleanup();
  });
  call.on('cancelled', cleanup);
}

/**
 * RPC Обработчик UploadAwgGoBinary (клиентский стрим RPC для загрузки бинарника amneziawg-go)
 */
export async function uploadAwgGoBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/amneziawg_go_${uniqueId}.tmp`;
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
      logger.warn('Unauthorized UploadAwgGoBinary attempt rejected');
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

    if (!secretVerified || bytesWritten === 0) {
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No valid binary data received or unauthorized.' });
    }

    try {
      const targetPath = config.AWG_GO_BINARY_PATH || '/usr/local/bin/amneziawg-go';
      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      await replaceBinaryAtomically(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});

      logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated amneziawg-go binary');
      invalidateAwgVersionCache();

      let serviceMsg = '';
      if (process.env.NODE_ENV !== 'test') {
        const restartRes = await restartActiveAwgServices();
        if (restartRes.reloaded) {
          serviceMsg = ' and active AWG service(s) restarted';
        } else {
          serviceMsg = ' (service restart skipped: not yet configured)';
        }
      }

      return callback(null, {
        success: true,
        message: `amneziawg-go binary version ${targetVersion} successfully updated${serviceMsg}`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded amneziawg-go binary');
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadAwgGoBinary stream');
    cleanup();
  });
  call.on('cancelled', cleanup);
}

/**
 * RPC Обработчик UploadCaddyBinary (клиентский стрим RPC для загрузки кастомно собранного
 * Caddy с плагином caddy-dns/cloudflare — только для Xeon-ring узлов, egress-ноды продолжают
 * использовать apt-Caddy и этот RPC никогда не получают).
 */
export async function uploadCaddyBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/caddy_custom_${uniqueId}.tmp`;
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
      logger.warn('Unauthorized UploadCaddyBinary attempt rejected');
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

    if (!secretVerified || bytesWritten === 0) {
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No valid binary data received or unauthorized.' });
    }

    try {
      const targetPath = config.CADDY_BINARY_PATH || '/usr/local/bin/caddy-custom';
      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      await replaceBinaryAtomically(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});

      logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated custom Caddy binary');

      // UploadCaddyBinary is only ever invoked for Xeon-ring nodes (regular egress nodes never
      // receive this RPC at all), so it's safe to switch the systemd unit over to the custom
      // binary right here — idempotent (no-op if the override already matches).
      let overrideMsg = '';
      if (process.env.NODE_ENV !== 'test') {
        await ensureCaddyCustomBinaryOverride();
        overrideMsg = ' and systemd override provisioned';
      }

      return callback(null, {
        success: true,
        message: `Custom Caddy binary version ${targetVersion} successfully updated${overrideMsg}`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded custom Caddy binary');
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadCaddyBinary stream');
    cleanup();
  });
  call.on('cancelled', cleanup);
}

