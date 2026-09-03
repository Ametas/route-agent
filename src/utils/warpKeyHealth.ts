import pino from 'pino';
import type { ClashApiEndpoint } from './singboxConnections.js';

const logger = pino({ level: 'info' });

/**
 * Здоровье WARP-ключей, прочитанное у ТЫЛОВОГО sing-box через его локальный Clash API.
 *
 * ЗАМЕРЫ ЗДЕСЬ НЕ ДЕЛАЮТСЯ, и это главное. Конфиг тыла собирает все WARP-туннели в группу
 * `urltest`, поэтому sing-box уже щупает каждый по собственному расписанию и держит результат.
 * Clash API отдаёт эти сохранённые замеры в `/proxies` — вместе с endpoint-ами, а не только с
 * outbound-ами (проверено по исходникам v1.14.0: `getProxies` дописывает `endpoint.Endpoints()` к
 * списку). Значит агенту остаётся прочитать уже измеренное, и трафика на пробу не тратится вовсе.
 *
 * ПУСТАЯ ИСТОРИЯ ЗНАЧИТ «НЕ ОТВЕТИЛ». Это не наше соглашение, а поведение sing-box: при неудаче
 * замера он УДАЛЯЕТ запись истории, при удаче — записывает. Поэтому отсутствие истории — сигнал, а
 * не отсутствие данных.
 */

/** Одна строка отчёта: один WARP-туннель. */
export interface WarpKeyHealthRecord {
  endpointTag: string;
  alive: boolean;
  rttMs: number;
  measuredAtUnixMs: number;
}

interface ClashHistoryEntry {
  time?: unknown;
  delay?: unknown;
}

interface ClashProxyEntry {
  history?: unknown;
}

/** Теги наших туннелей. Группа называется ровно `warp`, поэтому одного префикса мало. */
const WARP_TAG_PREFIX = 'warp-';

function parseHistory(raw: unknown): { delay: number; at: number } | null {
  if (!Array.isArray(raw) || raw.length === 0) return null;

  // Берём последнюю запись: sing-box дописывает свежие в конец.
  const last = raw[raw.length - 1] as ClashHistoryEntry;
  const delay = typeof last?.delay === 'number' ? last.delay : NaN;
  if (!Number.isFinite(delay) || delay <= 0) return null;

  // `time` приходит строкой RFC3339 от Go. Нераспознанное время не делает замер недействительным —
  // задержка тут важнее, — поэтому падаем на ноль, а не выбрасываем всю запись.
  const at = typeof last?.time === 'string' ? Date.parse(last.time) : NaN;
  return { delay, at: Number.isFinite(at) ? at : 0 };
}

export function parseWarpKeyHealth(payload: unknown): WarpKeyHealthRecord[] {
  const proxies = (payload as { proxies?: Record<string, ClashProxyEntry> })?.proxies;
  if (!proxies || typeof proxies !== 'object') return [];

  const records: WarpKeyHealthRecord[] = [];
  for (const [tag, entry] of Object.entries(proxies)) {
    // Строго префикс И непустой остаток: тег группы — ровно `warp`, и принять его за ключ значило
    // бы отчитаться о туннеле, которого нет.
    if (!tag.startsWith(WARP_TAG_PREFIX) || tag.length === WARP_TAG_PREFIX.length) continue;

    const measured = parseHistory(entry?.history);
    records.push({
      endpointTag: tag,
      alive: measured !== null,
      rttMs: measured?.delay ?? 0,
      measuredAtUnixMs: measured?.at ?? 0,
    });
  }
  return records;
}

export async function fetchWarpKeyHealth(endpoint: ClashApiEndpoint): Promise<WarpKeyHealthRecord[]> {
  const headers: Record<string, string> = endpoint.secret ? { authorization: `Bearer ${endpoint.secret}` } : {};

  const response = await fetch(`http://${endpoint.address}/proxies`, {
    headers,
    signal: AbortSignal.timeout(5000),
  });
  if (!response.ok) {
    throw new Error(`clash api responded ${response.status} for /proxies`);
  }

  const records = parseWarpKeyHealth(await response.json());
  logger.debug({ count: records.length }, 'Read WARP key health from the rear sing-box clash api');
  return records;
}
