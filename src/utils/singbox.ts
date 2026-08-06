import * as fs from 'fs/promises';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync, execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

const CADDY_LIB_DIR = '/var/lib/caddy';

/**
 * Гарантирует права на чтение сертификатов Caddy для ядра sing-box.
 *
 * Официальный apt-пакет caddy на Debian/Ubuntu всегда создаёт системного
 * пользователя/группу `caddy` и раскладывает `/var/lib/caddy` под ним —
 * это стандартное поведение пакета, а не особенность конкретной ноды,
 * поэтому владелец захардкожен, а не определяется через `getent`/`id`
 * (лишний exec и лишняя точка отказа ради значения, которое не варьируется
 * на поддерживаемых дистрибутивах). chmod сам по себе не чинит выданный
 * root:root каталог — процесс Caddy всегда работает от caddy:caddy
 * (systemctl show caddy -p User -p Group), поэтому chown обязателен;
 * chmod оставлен следом как дополнительная гарантия на случай, если
 * владелец уже корректен, но биты доступа слишком строгие.
 * chown -R рекурсивно переустанавливает владельца на всех уже существующих
 * файлах/подкаталогах при каждом вызове, поэтому одного этого фикса
 * достаточно, чтобы починить ранее испорченные ноды — без отдельной миграции.
 *
 * `dir`/`runExec` параметризованы только ради юнит-тестов (см. tests/app.test.ts);
 * все боевые вызовы используют значения по умолчанию.
 */
export async function fixCaddyPermissions(
  dir: string = CADDY_LIB_DIR,
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<void> {
  try {
    const caddyDirExists = await fs.stat(dir).then(() => true).catch(() => false);
    if (caddyDirExists) {
      await runExec(`chown -R caddy:caddy ${dir} || true`);
      await runExec(`chmod -R 755 ${dir} || true`);
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
