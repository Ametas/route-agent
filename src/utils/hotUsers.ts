import http from 'http';
import pino from 'pino';
import { getSingBoxVersion } from './telemetry.js';

const logger = pino({ level: 'info' });

/**
 * Горячая замена набора абонентов без перезагрузки ядра.
 *
 * ЗАЧЕМ. Reload у sing-box — это `instance.Close()` и сборка нового инстанса; частичной
 * перезагрузки в дереве нет вовсе (SagerNet/sing-box#3731). Поэтому правка ОДНОГО абонента рвала
 * сессии ВСЕМ на ноде. Замеряно на живом узле 2026-09-17: из 144 соединений не пережило ни одного.
 *
 * ЧЕМ ЗАМЕНЕНО. Наш форк sing-box (`1.14.0-hotusers`) поднимает службу `users-api` на unix-сокете:
 * `PUT /inbounds/<тег>/users` с ПОЛНЫМ списком абонентов инбаунда. Установленные соединения замену
 * переживают — это проверено на живом трафике для vless, tuic и hysteria2.
 *
 * ПОЧЕМУ РЕШАЕТ АГЕНТ, А НЕ ОРКЕСТРАТОР. Служба существует только в форке: сток отвергнет
 * неизвестный тип и не поднимется вовсе. Кто именно стоит на ноде, достоверно знает только сам
 * агент — у оркестратора это сведения из телеметрии, то есть в лучшем случае вчерашние.
 *
 * ЧЕГО ЭТО НЕ ДАЁТ. Немедленного отзыва. У tuic и hysteria2 клиент держит ОДНУ аутентифицированную
 * QUIC-сессию и гоняет по ней всё, новые запросы проверку не проходят — выведенный абонент
 * работает, пока держит сессию. Ровно поэтому в контракте есть `force_reload`: там, где доступ
 * отзывают, оркестратор просит порвать сессии явно.
 */

/** Путь сокета. Порта у ручки нет намеренно — см. README форка: нечего закрывать фаерволом. */
export const USERS_API_SOCKET_PATH = '/run/sing-box/users.sock';

const USERS_API_SERVICE = {
  type: 'users-api',
  tag: 'users',
  path: USERS_API_SOCKET_PATH,
} as const;

/** Суффикс версии, которым форк себя объявляет. Тот же, что оркестратор ищет у себя. */
const FORK_VERSION_SUFFIX = '-hotusers';

/**
 * Протоколы, у которых форк умеет менять набор на живую.
 *
 * Список закрытый и это важно: инбаунд другого типа ручка встретит 404, а узнать об этом ПОСЛЕ
 * того, как часть наборов уже применена, значит остаться в полуприменённом состоянии. Решение
 * принимается целиком до первой мутации.
 */
const HOT_SWAPPABLE_TYPES = new Set(['vless', 'tuic', 'hysteria2']);

interface InboundLike {
  type?: unknown;
  tag?: unknown;
  users?: unknown;
}

interface ConfigLike {
  inbounds?: unknown;
  services?: unknown;
}

/** Один набор, готовый к отправке. */
export interface RosterUpdate {
  tag: string;
  users: unknown[];
}

export async function isForkBinary(): Promise<boolean> {
  const version = await getSingBoxVersion();
  return version.endsWith(FORK_VERSION_SUFFIX);
}

/**
 * Добавляет службу `users-api` в конфиг, если её там ещё нет.
 *
 * Идемпотентно и ДО сверки с диском: иначе сверка сравнивала бы присланный конфиг без службы с
 * лежащим на диске со службой и не совпадала бы никогда.
 */
export function withUsersApiService(configObj: object): object {
  const config = configObj as ConfigLike;
  const services = Array.isArray(config.services) ? config.services : [];

  if (services.some((s) => (s as { type?: unknown }).type === USERS_API_SERVICE.type)) {
    return configObj;
  }

  return { ...configObj, services: [...services, { ...USERS_API_SERVICE }] };
}

function inboundsOf(configObj: object): InboundLike[] | null {
  const inbounds = (configObj as ConfigLike).inbounds;
  return Array.isArray(inbounds) ? (inbounds as InboundLike[]) : null;
}

/** Тот же конфиг без наборов абонентов — то, что обязано совпасть побайтово. */
function withoutUsers(configObj: object, inbounds: InboundLike[]): string {
  const stripped = inbounds.map((inbound) => {
    const copy = { ...inbound };
    delete copy.users;
    return copy;
  });
  return JSON.stringify({ ...configObj, inbounds: stripped });
}

/**
 * Что изменилось между конфигами: только наборы абонентов — или что-то ещё.
 *
 * `null` означает «горячим путём не пройти», и это ЕДИНСТВЕННЫЙ ответ на любую неясность: другой
 * диф, инбаунд без тега, инбаунд неподдерживаемого протокола, отсутствие `inbounds` вовсе. Пустой
 * массив — конфиги не различаются вовсе.
 */
export function planRosterUpdate(nextConfig: object, currentConfig: object): RosterUpdate[] | null {
  const nextInbounds = inboundsOf(nextConfig);
  const currentInbounds = inboundsOf(currentConfig);
  if (!nextInbounds || !currentInbounds) return null;

  // Всё, кроме наборов, обязано совпасть побайтово. Сравнение заодно ловит добавленные, убранные и
  // переставленные инбаунды: у них расходится уже эта часть.
  if (withoutUsers(nextConfig, nextInbounds) !== withoutUsers(currentConfig, currentInbounds)) {
    return null;
  }

  const updates: RosterUpdate[] = [];

  for (let i = 0; i < nextInbounds.length; i++) {
    const next = nextInbounds[i];
    const current = currentInbounds[i];
    if (JSON.stringify(next.users ?? null) === JSON.stringify(current.users ?? null)) continue;

    if (typeof next.type !== 'string' || !HOT_SWAPPABLE_TYPES.has(next.type)) return null;
    if (typeof next.tag !== 'string' || next.tag === '') return null;
    if (!Array.isArray(next.users)) return null;

    updates.push({ tag: next.tag, users: next.users });
  }

  return updates;
}

/**
 * Отправляет ПОЛНЫЙ набор одного инбаунда в живое ядро.
 *
 * Коды ручка различает намеренно: `400` — набор негодный, `404` — нет такого инбаунда или его
 * протокол так не умеет. Оба означают, что горячим путём не вышло, и оба обязаны поднять исключение
 * — вызывающий на него откатится к перезагрузке.
 */
export async function putRoster(update: RosterUpdate, socketPath = USERS_API_SOCKET_PATH): Promise<void> {
  const body = JSON.stringify(update.users);

  await new Promise<void>((resolve, reject) => {
    const request = http.request(
      {
        socketPath,
        method: 'PUT',
        path: `/inbounds/${encodeURIComponent(update.tag)}/users`,
        headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) },
      },
      (response) => {
        const chunks: Buffer[] = [];
        response.on('data', (chunk: Buffer) => chunks.push(chunk));
        response.on('end', () => {
          if (response.statusCode === 200) return resolve();
          const detail = Buffer.concat(chunks).toString('utf-8').trim();
          reject(new Error(`users-api ответила ${response.statusCode} на инбаунд ${update.tag}: ${detail}`));
        });
      }
    );

    request.on('error', reject);
    request.end(body);
  });
}

/**
 * Раскатывает наборы по инбаундам. Любая неудача — исключение: вызывающий откатится к reload,
 * который применит уже записанный на диск конфиг целиком и тем самым сойдётся.
 */
export async function applyRosterUpdates(updates: RosterUpdate[]): Promise<void> {
  for (const update of updates) {
    await putRoster(update);
    logger.info(
      { tag: update.tag, users: update.users.length },
      'Набор абонентов инбаунда заменён на живую, без перезагрузки ядра'
    );
  }
}
