import { ServerReadableStream, ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import * as fs from 'fs/promises';
import { createWriteStream } from 'fs';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync, execFileAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';
import { invalidateSingboxVersionCache } from '../utils/telemetry.js';

const logger = pino({ level: 'info' });

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

      await fs.mkdir(path.dirname(targetPath), { recursive: true });
      await fs.rename(tempPath, targetPath).catch(async () => {
        await fs.copyFile(tempPath, targetPath);
        await fs.unlink(tempPath).catch(() => {});
      });

      logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated sing-box binary');
      invalidateSingboxVersionCache();

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

      await fs.mkdir(path.dirname(targetPath), { recursive: true });
      await fs.rename(tempPath, targetPath).catch(async () => {
        await fs.copyFile(tempPath, targetPath);
        await fs.unlink(tempPath).catch(() => {});
      });

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

    await fs.mkdir(path.dirname(targetPath), { recursive: true });
    await fs.rename(tempPath, targetPath).catch(async () => {
      await fs.copyFile(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});
    });

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
