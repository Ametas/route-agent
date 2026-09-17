import pino from 'pino';
import { readActiveRearClashApi } from './rearCore.js';
import type { SingBoxConnectionRecord } from './singboxConnections.js';

const logger = pino({ level: 'info' });

/**
 * Соединения тылового ядра (mihomo) — единственное место, где видно НАСТОЯЩЕЕ назначение.
 *
 * **ПОЧЕМУ ИМЕННО ТЫЛ.** У фронта назначение — это кольцо, у эгресса — петля на локальный порт
 * (`127.0.0.1:20005`). Ни тот, ни другой внешнего адреса не знают. Тыл стоит на хоп дальше и
 * МАРШРУТИЗИРУЕТ по реальному адресу — значит знает его точно, а не по сниффингу наугад.
 *
 * **МЕТАДАННЫЕ У MIHOMO БОГАЧЕ, ЧЕМ У SING-BOX.** Замер на живом узле: против девяти полей
 * sing-box здесь есть `inboundName`, `inboundUser`, `destinationIPASN`, `sniffHost`, `chains`.
 * Из них нам нужны назначение, порт, байты по направлениям и имя инбаунда — последнее наконец
 * заполняет `inbound_tag`, который у sing-box всегда пуст (проверено: 116 соединений, в том числе
 * 18 через tuic-инбаунды с заведёнными пользователями, — поля `user` нет ни в одном).
 *
 * **АБОНЕНТА ЗДЕСЬ НЕТ, И ЭТО НЕ ПРОБЕЛ СБОРА.** Эгресс ходит в тыл одним исходящим VLESS с одним
 * uuid на всех, поэтому `inboundUser` у всех записей одинаков (`link`). Разбор по этим записям
 * поэтому агрегатный: «куда льётся поток с узла», а не «кто льёт».
 */

/** Сырая запись Clash API mihomo. Поля, которых мы не читаем, намеренно не описаны. */
interface RawMihomoConnection {
  id?: unknown;
  upload?: unknown;
  download?: unknown;
  start?: unknown;
  metadata?: {
    network?: unknown;
    destinationIP?: unknown;
    destinationPort?: unknown;
    host?: unknown;
    sniffHost?: unknown;
    sourceIP?: unknown;
    inboundName?: unknown;
    inboundUser?: unknown;
  };
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown): number {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  if (typeof value === 'string') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

function parseStart(value: unknown): number {
  if (typeof value !== 'string' || value === '') return 0;
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? 0 : parsed;
}

/**
 * Разбор ответа mihomo в общую форму записи.
 *
 * Чистая функция: проверяется без живого ядра. Форма ответа снята с боевого узла, а не выведена
 * из документации, — ровно потому, что на выдуманных именах полей мы уже обожглись (`meta.user` и
 * `meta.inboundTag` у sing-box читались годами и не существовали).
 */
export function parseMihomoConnections(payload: unknown): SingBoxConnectionRecord[] {
  const list = (payload as { connections?: unknown })?.connections;
  if (!Array.isArray(list)) return [];

  const records: SingBoxConnectionRecord[] = [];
  for (const raw of list as RawMihomoConnection[]) {
    const meta = raw?.metadata ?? {};
    records.push({
      id: asString(raw?.id),
      // `inboundUser` — общий для всех абонентов uuid связки, а не конкретный человек. Переносим
      // как есть: подменять его пустой строкой значило бы скрыть, что поле вообще заполняется.
      user: asString(meta.inboundUser),
      sourceIp: asString(meta.sourceIP),
      destinationIp: asString(meta.destinationIP),
      // `host` — то, что клиент попросил; `sniffHost` — то, что ядро вынюхало из трафика. Первое
      // точнее, когда есть; второе спасает соединения, пришедшие адресом без имени.
      destinationDomain: asString(meta.host) || asString(meta.sniffHost),
      destinationPort: asNumber(meta.destinationPort),
      uploadBytes: asNumber(raw?.upload),
      downloadBytes: asNumber(raw?.download),
      network: asString(meta.network),
      inboundTag: asString(meta.inboundName),
      startedAtUnixMs: parseStart(raw?.start),
      closed: false,
    });
  }
  return records;
}

/**
 * Снимок соединений тыла.
 *
 * Тыл поднят не на каждом узле — на узле без него это штатное состояние, а не отказ, поэтому
 * `null` от `readActiveRearClashApi` даёт пустую выборку без шума в логе.
 */
export async function collectRearConnections(): Promise<SingBoxConnectionRecord[]> {
  const endpoint = await readActiveRearClashApi();
  if (!endpoint) {
    logger.debug('Rear core is not active on this node, no connections to report');
    return [];
  }

  const response = await fetch(`http://${endpoint.address}/connections`, {
    headers: endpoint.secret ? { authorization: `Bearer ${endpoint.secret}` } : {},
    signal: AbortSignal.timeout(5000),
  });
  if (!response.ok) {
    throw new Error(`rear clash api responded ${response.status}`);
  }

  return parseMihomoConnections(await response.json());
}
