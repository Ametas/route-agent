import * as fs from 'fs/promises';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import { config } from '../config.js';
import { authenticateCall } from '../middleware/auth.js';
import { execAsync } from '../utils/exec.js';
import { validateSingBoxConfig } from '../utils/singbox.js';

const logger = pino({ level: 'info' });

/**
 * ТЫЛОВОЙ инстанс sing-box — второй процесс sing-box на этой же ноде.
 *
 * Фронтовой (`config.service.ts`) терминирует абонентов; тыловой решает, куда уходит их трафик, и
 * служит точкой наблюдения за направлениями. Конфиг для него собирает ОРКЕСТРАТОР целиком: в нём
 * лежат приватные ключи WARP из его пула, и учёт «какой ключ какой ноде» ведётся только там.
 *
 * ОДИН БИНАРЬ, ДВА ЮНИТА. Тыл запускается из того же `sing-box`, что и фронт, — второго пути
 * установки нет намеренно, чтобы `UpgradeSingbox` и `SelfUpdate` покрывали оба инстанса и не
 * появилось версии, о которой никто не знает. Отсюда жёсткое предусловие: нет бинаря — нет и
 * тыла, и мы отвечаем `skipped_reason`, а не ошибкой.
 */

function rearConfigPath(): string {
  return config.REAR_SINGBOX_CONFIG_PATH || '/etc/sing-box/rear.json';
}

function rearUnitPath(): string {
  return config.REAR_SINGBOX_UNIT_FILE_PATH || '/etc/systemd/system/route-rear-singbox.service';
}

function rearUnitName(): string {
  return path.basename(rearUnitPath(), '.service');
}

/**
 * Идемпотентно создаёт/обновляет unit тыла. Пишет только при отличии — иначе каждый пуш конфига
 * дёргал бы `daemon-reload` без всякой причины (тот же приём, что в `ensureAwgSystemdUnit`).
 */
async function ensureRearUnit(): Promise<void> {
  const unitPath = rearUnitPath();
  const binary = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';

  const expected = `[Unit]
Description=Rear sing-box (WARP egress, managed by route-agent)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${binary} run -c ${rearConfigPath()}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`;

  const existing = await fs.readFile(unitPath, 'utf-8').catch(() => null);
  if (existing === expected) return;

  await fs.mkdir(path.dirname(unitPath), { recursive: true });
  await fs.writeFile(unitPath, expected, 'utf-8');
  logger.info({ path: unitPath }, 'Provisioned/updated rear sing-box systemd unit file');

  if (process.env.NODE_ENV !== 'test') {
    await execAsync('systemctl daemon-reload').catch((err: any) => {
      logger.warn({ err: err.message }, 'Failed to daemon-reload after writing rear sing-box unit');
    });
  }
}

/** Атомарная запись конфига тыла: временный файл рядом, затем rename. */
async function writeRearConfig(configObj: object): Promise<void> {
  const target = rearConfigPath();
  const dir = path.dirname(target);
  const tmp = path.join(dir, `.rear.${Date.now()}_${crypto.randomUUID().slice(0, 8)}.tmp`);

  await fs.mkdir(dir, { recursive: true });
  await fs.writeFile(tmp, JSON.stringify(configObj, null, 2), 'utf-8');
  await fs.rename(tmp, target);
}

/** Читаем состояние У ЮНИТА, а не выводим из того, что команда не упала. */
async function isRearRunning(): Promise<boolean> {
  if (process.env.NODE_ENV === 'test') return true;
  const { stdout } = await execAsync(`systemctl is-active ${rearUnitName()}`).catch(() => ({ stdout: '' }));
  return stdout.trim() === 'active';
}

async function stopRear(): Promise<void> {
  if (process.env.NODE_ENV === 'test') return;
  const unit = rearUnitName();
  await execAsync(`systemctl disable --now ${unit}`).catch(() => {});
}

/**
 * RPC-обработчик ConfigureRearSingbox.
 *
 * Порядок здесь — половина смысла. `sing-box check` идёт ДО того, как мы тронем живой конфиг:
 * записать неверный и перезапуститься означало бы уронить работающий тыл, и ветка, шедшая через
 * WARP, не деградировала бы, а встала. Поэтому при отказе проверки прежний конфиг остаётся на
 * месте нетронутым, а оркестратор получает `check_failed`.
 */
export async function configureRearSingboxHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized ConfigureRearSingbox request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', running: false });
  }

  const enabled = Boolean(call.request.enabled);
  const rawConfig = call.request.configJson || call.request.config_json || '';

  try {
    if (!enabled) {
      await stopRear();
      // Конфиг убираем вместе с инстансом: в нём лежат приватные ключи WARP, и оставлять их на
      // ноде, которая больше не в пуле, незачем.
      await fs.unlink(rearConfigPath()).catch(() => {});
      await fs.unlink(rearUnitPath()).catch(() => {});
      logger.info('Rear sing-box stopped and removed');
      return callback(null, { success: true, message: 'Rear sing-box stopped and removed.', running: false });
    }

    const binary = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
    const hasBinary = await fs.stat(binary).then(() => true).catch(() => false);
    if (!hasBinary && process.env.NODE_ENV !== 'test') {
      logger.warn({ binary }, 'ConfigureRearSingbox requested but sing-box is not installed');
      return callback(null, {
        success: false,
        message: 'sing-box is not installed on this node — nothing to run a rear instance from.',
        running: false,
        skipped_reason: 'singbox_not_installed',
      });
    }

    const configObj = JSON.parse(rawConfig);

    const syntaxCheck = await validateSingBoxConfig(configObj);
    if (!syntaxCheck.valid) {
      logger.error({ err: syntaxCheck.error }, 'Rear sing-box config rejected by sing-box check');
      return callback(null, {
        success: false,
        message: `Rejected by Node Agent: invalid rear sing-box syntax. Error: ${syntaxCheck.error}`,
        running: await isRearRunning(),
        skipped_reason: 'check_failed',
      });
    }

    await writeRearConfig(configObj);
    await ensureRearUnit();

    if (process.env.NODE_ENV !== 'test') {
      // `enable --now` и на первом включении, и на последующих: юнит уже включён — команда
      // идемпотентна, а перезапуск нужен всегда, потому что конфиг изменился.
      await execAsync(`systemctl enable --now ${rearUnitName()}`);
      await execAsync(`systemctl restart ${rearUnitName()}`);
    }

    const running = await isRearRunning();
    logger.info({ running }, 'Rear sing-box configuration applied');
    return callback(null, {
      success: true,
      message: 'Rear sing-box configuration validated, applied and (re)started.',
      running,
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to process ConfigureRearSingbox RPC pipeline');
    return callback(null, { success: false, message: `Internal Agent Error: ${msg}`, running: false });
  }
}
