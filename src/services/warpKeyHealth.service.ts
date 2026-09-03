import * as fs from 'fs/promises';
import pino from 'pino';
import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import { config } from '../config.js';
import { authenticateCall } from '../middleware/auth.js';
import { readClashApiEndpoint } from '../utils/singboxConnections.js';
import { fetchWarpKeyHealth } from '../utils/warpKeyHealth.js';

const logger = pino({ level: 'info' });

/**
 * RPC-обработчик GetWarpKeyHealth.
 *
 * Читает у ТЫЛОВОГО инстанса sing-box его же сохранённые замеры задержки по каждому WARP-туннелю.
 * Своих замеров не делает — см. `utils/warpKeyHealth.ts`.
 *
 * Три исхода различаются намеренно, потому что оркестратор поступает с ними по-разному:
 *   * `rear_not_running` — тыла на этой ноде нет. Не ошибка: WARP на ней просто не включён.
 *   * `clash_api_unreachable` — тыл есть, но его API молчит. Вот это уже повод посмотреть.
 *   * пустой список БЕЗ причины пропуска — тыл работает, WARP-туннелей в нём пока нет. Нормальное
 *     состояние, пока пул ключей заполняется.
 *
 * Слить их в одно «ничего не отдали» значило бы, что оркестратор не сможет отличить «нечего
 * измерять» от «измерять некому», и в обоих случаях либо молчал бы, либо шумел.
 */
export async function getWarpKeyHealthHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetWarpKeyHealth request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', entries: [] });
  }

  const rearConfigPath = config.REAR_SINGBOX_CONFIG_PATH || '/etc/sing-box/rear.json';

  try {
    const exists = await fs.stat(rearConfigPath).then(() => true).catch(() => false);
    if (!exists) {
      return callback(null, {
        success: true,
        message: 'Rear sing-box is not configured on this node.',
        entries: [],
        skippedReason: 'rear_not_running',
      });
    }

    // Адрес и секрет берутся из ПРИМЕНЁННОГО конфига тыла, а не из отдельной настройки: так они не
    // могут разойтись с тем, что на самом деле слушает процесс.
    const endpoint = await readClashApiEndpoint(rearConfigPath);
    if (!endpoint) {
      return callback(null, {
        success: true,
        message: 'Rear sing-box config carries no clash_api endpoint.',
        entries: [],
        skippedReason: 'rear_not_running',
      });
    }

    const entries = await fetchWarpKeyHealth(endpoint);
    return callback(null, {
      success: true,
      message: `Reported health for ${entries.length} WARP endpoint(s).`,
      entries: entries.map((entry) => ({
        endpointTag: entry.endpointTag,
        alive: entry.alive,
        rttMs: entry.rttMs,
        measuredAtUnixMs: entry.measuredAtUnixMs,
      })),
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg }, 'Failed to read WARP key health from the rear sing-box');
    return callback(null, {
      success: false,
      message: `Could not read rear clash api: ${msg}`,
      entries: [],
      skippedReason: 'clash_api_unreachable',
    });
  }
}
