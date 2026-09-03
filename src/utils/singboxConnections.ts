import fs from 'node:fs/promises';
import pino from 'pino';
import { config } from '../config.js';

const logger = pino({ level: 'info' });

/**
 * Чтение записей о соединениях из локального Clash API sing-box'а (2026-08-30).
 *
 * **Почему Clash API, а не прежний v2ray_api.** На ноды ставится официальный релизный бинарь
 * sing-box, а он собирается по `release/DEFAULT_BUILD_TAGS`, где `with_v2ray_api` отсутствует,
 * а `with_clash_api` есть. Без тега sing-box отвергает конфиг с блоком `v2ray_api` целиком — то
 * есть прежний канал не просто молчал, он ронял применение конфига на любой ноде, где появлялся
 * первый tuic/hy2-пользователь.
 *
 * **И он отвечает на больший вопрос.** v2ray_api давал только «сколько байт у пользователя». Здесь
 * на каждое соединение известны источник, назначение, пользователь и байты в обе стороны. Из этого
 * выводится и прежний ответ, и два новых: откуда идёт нагрузка и куда.
 *
 * Эта же RPC обслуживает и egress-ноды, и Xeon-фронты — агент на них один. На фронте поле
 * пользователя будет пустым (слепой SNI-релей никого не аутентифицирует), зато адрес источника
 * настоящий; на эгрессе за кольцом наоборот. Разбирать эту разницу — задача оркестратора.
 */

export interface SingBoxConnectionRecord {
  /**
   * Идентификатор соединения из Clash API. Наружу по gRPC не уезжает — он нужен пер-юзерным
   * счётчикам (utils/singboxStats.ts), которые считают приращение по каждому соединению между
   * проходами. Без стабильного ключа отличить «то же соединение накачало ещё» от «появилось новое»
   * невозможно, а на этом различии стоит весь подсчёт дельты.
   */
  id: string;
  user: string;
  sourceIp: string;
  destinationIp: string;
  destinationDomain: string;
  destinationPort: number;
  uploadBytes: number;
  downloadBytes: number;
  network: string;
  inboundTag: string;
  startedAtUnixMs: number;
  closed: boolean;
}

export interface ClashApiEndpoint {
  address: string;
  secret: string;
}

/**
 * Достаёт адрес и секрет Clash API из ПРИМЕНЁННОГО конфига, а не из собственных настроек агента.
 *
 * Так источник правды один: что реально работает на ноде, то и опрашиваем. Агент не хранит копию
 * этих значений и не может с ней разойтись — при любой перегенерации конфига он читает новые.
 * Ровно тот же приём использовался для v2ray_api.
 */
export async function readClashApiEndpoint(
  configPath: string = config.SINGBOX_CONFIG_PATH
): Promise<ClashApiEndpoint | null> {
  try {
    const raw = await fs.readFile(configPath, 'utf-8');
    const parsed = JSON.parse(raw);
    const api = parsed?.experimental?.clash_api;
    const address = api?.external_controller;
    if (typeof address !== 'string' || address.trim() === '') return null;
    return {
      address: address.trim(),
      secret: typeof api?.secret === 'string' ? api.secret : '',
    };
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.debug({ err: msg }, 'Could not read live sing-box config to resolve the clash_api endpoint');
    return null;
  }
}

/** Форма записи, которую отдаёт Clash API. Проверяем каждое поле — доверять форме нельзя. */
interface RawClashConnection {
  id?: unknown;
  upload?: unknown;
  download?: unknown;
  start?: unknown;
  metadata?: {
    network?: unknown;
    sourceIP?: unknown;
    destinationIP?: unknown;
    destinationPort?: unknown;
    host?: unknown;
    inboundTag?: unknown;
    user?: unknown;
  };
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown): number {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  // Clash API отдаёт порт строкой, а счётчики — числами. Приводим обе формы, но молча роняем мусор
  // в ноль вместо NaN: одна кривая запись не должна портить весь ответ.
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
 * Превращает ответ Clash API в плоские записи.
 *
 * Отделено от сетевого вызова намеренно: это единственная часть с логикой, и её надо уметь
 * проверить тестом без живого sing-box'а.
 */
export function parseClashConnections(payload: unknown, closed: boolean): SingBoxConnectionRecord[] {
  const list = (payload as { connections?: unknown })?.connections;
  if (!Array.isArray(list)) return [];

  const records: SingBoxConnectionRecord[] = [];
  for (const raw of list as RawClashConnection[]) {
    const meta = raw?.metadata ?? {};
    records.push({
      id: asString(raw?.id),
      user: asString(meta.user),
      sourceIp: asString(meta.sourceIP),
      destinationIp: asString(meta.destinationIP),
      destinationDomain: asString(meta.host),
      destinationPort: asNumber(meta.destinationPort),
      uploadBytes: asNumber(raw?.upload),
      downloadBytes: asNumber(raw?.download),
      network: asString(meta.network),
      inboundTag: asString(meta.inboundTag),
      startedAtUnixMs: parseStart(raw?.start),
      closed,
    });
  }
  return records;
}

/**
 * Запрашивает у Clash API живые и недавно завершённые соединения.
 *
 * Завершённые берутся намеренно: свип ходит раз в несколько минут, а короткие потоки за это время
 * успевают открыться и закрыться целиком. Без них картина систематически смещалась бы в сторону
 * долгих соединений — то есть ровно мимо всплесков, ради которых всё и затевалось.
 *
 * Сбой любой из двух половин не отменяет другую: недоступный буфер завершённых не повод потерять
 * живые.
 */
export async function fetchConnections(endpoint: ClashApiEndpoint): Promise<SingBoxConnectionRecord[]> {
  const headers: Record<string, string> = endpoint.secret
    ? { authorization: `Bearer ${endpoint.secret}` }
    : {};

  const get = async (path: string, closed: boolean): Promise<SingBoxConnectionRecord[]> => {
    const response = await fetch(`http://${endpoint.address}${path}`, {
      headers,
      signal: AbortSignal.timeout(5000),
    });
    if (!response.ok) {
      throw new Error(`clash api responded ${response.status} for ${path}`);
    }
    return parseClashConnections(await response.json(), closed);
  };

  const [live, closed] = await Promise.allSettled([
    get('/connections', false),
    get('/connections?closed=true', true),
  ]);

  const records: SingBoxConnectionRecord[] = [];
  if (live.status === 'fulfilled') {
    records.push(...live.value);
  } else {
    // Живые соединения — основная половина; если не отдались они, это уже повод сказать наружу.
    throw live.reason instanceof Error ? live.reason : new Error(String(live.reason));
  }
  if (closed.status === 'fulfilled') {
    records.push(...closed.value);
  } else {
    const msg = closed.reason instanceof Error ? closed.reason.message : String(closed.reason);
    logger.debug({ err: msg }, 'Closed-connections buffer unavailable, returning live connections only');
  }

  return records;
}
