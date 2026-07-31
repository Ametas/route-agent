import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync } from '../utils/exec.js';

const logger = pino({ level: 'info' });

/**
 * Идемпотентно создаёт или обновляет systemd unit-файл для управления AWG-интерфейсом.
 * Записывает файл только если он отсутствует или его содержимое отличается.
 * При создании или изменении файла выполняет systemctl daemon-reload.
 */
export async function ensureAwgSystemdUnit(overridePath?: string): Promise<boolean> {
  const unitPath = overridePath || config.AWG_UNIT_FILE_PATH || '/etc/systemd/system/route-awg@.service';
  const awgBin = config.AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg';
  const awgQuickBin = `${awgBin}-quick`;

  const expectedContent = `[Unit]
Description=AmneziaWG interface %i (managed by route-agent)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=${awgQuickBin} up %i
ExecStop=${awgQuickBin} down %i
ExecReload=/bin/sh -c '${awgBin} syncconf %i <(${awgQuickBin} strip %i)'

[Install]
WantedBy=multi-user.target
`;

  try {
    const existingContent = await fs.readFile(unitPath, 'utf-8').catch(() => null);

    if (existingContent === expectedContent) {
      logger.debug({ path: unitPath }, 'AWG systemd unit file is already up to date; skipping write and daemon-reload');
      return false;
    }

    await fs.mkdir(path.dirname(unitPath), { recursive: true });
    await fs.writeFile(unitPath, expectedContent, 'utf-8');
    logger.info({ path: unitPath }, 'Provisioned/updated AWG systemd unit file');

    if (process.env.NODE_ENV !== 'test') {
      try {
        await execAsync('systemctl daemon-reload');
        logger.info('Executed systemctl daemon-reload after updating AWG systemd unit file');
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to execute systemctl daemon-reload');
      }
    }

    return true;
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg, path: unitPath }, 'Failed to ensure AWG systemd unit file');
    return false;
  }
}
