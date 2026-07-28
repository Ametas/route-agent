import * as fs from 'fs/promises';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync, execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Гарантирует права на чтение сертификатов Caddy для ядра sing-box
 */
export async function fixCaddyPermissions(): Promise<void> {
  try {
    const caddyDirExists = await fs.stat('/var/lib/caddy').then(() => true).catch(() => false);
    if (caddyDirExists) {
      await execAsync('chmod -R 755 /var/lib/caddy || true');
    }
  } catch (err: any) {
    logger.warn({ err: err.message }, 'Failed to adjust Caddy certificates permissions');
  }
}

/**
 * Вспомогательный метод локальной валидации синтаксиса sing-box перед его применением
 */
export async function validateSingBoxConfig(configObj: object): Promise<{ valid: boolean; error?: string }> {
  if (process.env.NODE_ENV === 'test') {
    return { valid: true };
  }
  await fixCaddyPermissions();
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const checkFilePath = path.join(targetDir, `.config.check_${uniqueId}.json`);
  const binaryPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';

  try {
    await fs.mkdir(targetDir, { recursive: true });
    await fs.writeFile(checkFilePath, JSON.stringify(configObj, null, 2), 'utf-8');

    // Выполняем нативный тест синтаксиса sing-box
    await execFileAsync(binaryPath, ['check', '-c', checkFilePath]);
    return { valid: true };
  } catch (err: any) {
    logger.error({ stderr: err.stderr }, 'Sing-box configuration syntax check failed');
    return { valid: false, error: err.stderr || err.message };
  } finally {
    await fs.unlink(checkFilePath).catch(() => {});
  }
}

/**
 * Исполнитель применения конфигурации и мягкой перезагрузки ядра
 */
export async function atomicApplyAndReload(configObj: object): Promise<void> {
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempFilePath = path.join(targetDir, `.config.${uniqueId}.tmp`);
  const backupFilePath = `${config.SINGBOX_CONFIG_PATH}.bak`;

  // 1. Сохраняем бэкап текущей конфигурации при её наличии
  const configExists = await fs.stat(config.SINGBOX_CONFIG_PATH).then(() => true).catch(() => false);
  if (configExists) {
    await fs.copyFile(config.SINGBOX_CONFIG_PATH, backupFilePath).catch(() => {});
  }

  // 2. Атомарная подмена через временный файл
  await fs.writeFile(tempFilePath, JSON.stringify(configObj, null, 2), 'utf-8');
  await fs.rename(tempFilePath, config.SINGBOX_CONFIG_PATH);

  // 3. Мягкий reload сервиса с откатом при ошибке
  if (process.env.NODE_ENV !== 'test' || process.env.RELOAD_COMMAND) {
    try {
      const { stdout, stderr } = await execAsync(config.RELOAD_COMMAND);
      if (stdout) logger.info({ stdout }, 'Reload command stdout');
      if (stderr) logger.warn({ stderr }, 'Reload command stderr');
    } catch (err) {
      if (configExists) {
        await fs.copyFile(backupFilePath, config.SINGBOX_CONFIG_PATH).catch(() => {});
      }
      throw err;
    }
  }
}
