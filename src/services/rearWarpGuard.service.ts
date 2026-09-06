import * as fs from 'fs/promises';
import pino from 'pino';
import { config } from '../config.js';
import { readActiveRearClashApi } from '../utils/rearCore.js';
import {
  currentSelection,
  decideWarpSelection,
  selectWarpMember,
  WARP_SELECTOR_TAG,
} from '../utils/rearWarpGuard.js';

const logger = pino({ level: 'info' });

/**
 * Периодический сторож WARP-ветки тыла — см. `utils/rearWarpGuard.ts` о том, зачем он вообще.
 *
 * Цикл намеренно ЧАСТЫЙ и очень дешёвый: один HTTP-запрос по loopback к соседнему процессу. Смысл
 * в скорости реакции — оркестратор доберётся до той же новости через час, а тут секунды.
 */

let timer: NodeJS.Timeout | null = null;
let running = false;

/**
 * Один проход. Возвращает выбранного члена, если что-то переключили, иначе `null`.
 *
 * Переключаем ТОЛЬКО при расхождении: селектор и так хранит выбор, а лишний PUT на каждом тике
 * означал бы запись в лог раз в полминуты и ничего больше.
 */
export async function guardOnce(): Promise<string | null> {
  // Ядро тыла определяется по тому, чей конфиг лежит на диске, а не берётся из настройки:
  // сторож работает локально и каждые полминуты, а какое ядро выбрано — знает только
  // оркестратор и только в момент настройки. Форма конфига у ядер разная, поэтому адрес Clash
  // API читает само описание ядра.
  const active = await readActiveRearClashApi();
  if (!active) return null;
  const endpoint = { address: active.address, secret: active.secret };

  const response = await fetch(`http://${endpoint.address}/proxies`, {
    headers: endpoint.secret ? { authorization: `Bearer ${endpoint.secret}` } : {},
    signal: AbortSignal.timeout(5000),
  });
  if (!response.ok) {
    throw new Error(`clash api responded ${response.status} for /proxies`);
  }

  const payload = await response.json();
  // Имена членов селектора у ядер РАЗНЫЕ: у sing-box это `wg-pool`/`direct`, у mihomo —
  // `warp-auto`/`DIRECT`. PUT с чужим именем отвергается, то есть сторож молча перестал бы
  // работать — ровно тот отказ, который здесь и стерегут.
  const decision = decideWarpSelection(payload, active.core.warpMembers);
  // Селектора нет — WARP на ноде не включён или пул ещё пуст. Вмешиваться не во что.
  if (!decision) return null;

  if (currentSelection(payload) === decision.desired) return null;

  await selectWarpMember(endpoint, decision.desired);
  logger.warn(
    { selector: WARP_SELECTOR_TAG, to: decision.desired, aliveKeys: decision.aliveKeys, totalKeys: decision.totalKeys },
    decision.desired === 'direct'
      ? 'Rear WARP branch fell back to direct: no live keys left'
      : 'Rear WARP branch restored: live keys are back'
  );
  return decision.desired;
}

export function startRearWarpGuard(): void {
  if (timer) return;

  const tick = async (): Promise<void> => {
    // Проходы не наслаиваются: предыдущий мог зависнуть на неотвечающем API.
    if (running) return;
    running = true;
    try {
      await guardOnce();
    } catch (err: unknown) {
      // Молчим на уровне debug: тыл может перезапускаться, и шуметь на каждом тике незачем.
      logger.debug({ err: err instanceof Error ? err.message : String(err) }, 'Rear WARP guard pass failed');
    } finally {
      running = false;
    }
  };

  timer = setInterval(() => {
    void tick();
  }, config.REAR_WARP_GUARD_INTERVAL_MS);
  timer.unref?.();
  logger.info({ intervalMs: config.REAR_WARP_GUARD_INTERVAL_MS }, 'Rear WARP guard started');
}

export function stopRearWarpGuard(): void {
  if (!timer) return;
  clearInterval(timer);
  timer = null;
}
