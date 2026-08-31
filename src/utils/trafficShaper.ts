import fs from 'node:fs/promises';
import path from 'node:path';
import pino from 'pino';
import { execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Ограничение скорости по сетевым префиксам источника (2026-08-31).
 *
 * **Полисер на ВХОДЯЩЕМ направлении, а не шейпер на исходящем.** Метка на входящем пакете не
 * переходит на исходящий сокет: sing-box терминирует соединение и открывает к назначению новое
 * (ровно поэтому у него есть собственный `routing_mark`). Значит по источнику ограничивается плечо
 * «клиент → мы». Этого достаточно — не дав абоненту протолкнуть в нас больше, мы ограничиваем и
 * дальнейшее плечо, — и исполняется это полисером, который лишнее ОТБРАСЫВАЕТ.
 *
 * **Отбрасывание здесь не побочный эффект, а половина смысла.** Проверено на живом ядре: под одним
 * и тем же полисером за одинаковое время кооперативный TCP дал 122 отброшенных пакета, а
 * некооперативный UDP-флуд — 1 791 515 (99,76% от отправленного, и он продолжал долбить на полной
 * скорости). Разница в четыре порядка. «Продолжает ломиться, несмотря на потери» отличает флуд от
 * крупной закачки поведением, а не объёмом — и узнать это можно, только приложив обратное давление.
 *
 * **Потолок задаётся НА ПРЕФИКС.** У лестницы эскалации две ступени: мягкий потолок, под которым
 * собираются улики, и жёсткий — для того, кто сквозь мягкий продолжил ломиться. Полисер в `tc` и
 * так настраивается на каждый фильтр отдельно, так что один потолок на весь список сделал бы
 * исполнителя неспособным выразить то, что решает лестница.
 *
 * **Почему без iptables.** `tc filter u32` умеет сопоставлять адрес источника сам, так что цепочка
 * с пометками не нужна вовсе: одно правило на префикс. Меньше движущихся частей, и файрвол ноды не
 * трогается совсем.
 */

/**
 * Куда агент записывает применённую раскладку. Состояние принадлежит агенту, потому что счётчики
 * `tc` привязаны к приоритету фильтра, а не к тексту префикса, — сопоставить их обратно можно
 * только зная, что именно применялось.
 */
const STATE_FILE = '/etc/route-agent/traffic-throttle.json';

/** Дескриптор ingress-qdisc'а. Значение фиксированное, так его принято адресовать в tc. */
const INGRESS_HANDLE = 'ffff:';

/** Один префикс со своим потолком. */
export interface ThrottleEntry {
  prefix: string;
  rate: string;
  burst: string;
}

export interface ThrottleState {
  interface: string;
  /** Порядок значим: индекс + 1 — это `prio` фильтра, по которому потом читаются счётчики. */
  entries: ThrottleEntry[];
}

export interface ThrottleCounter {
  prefix: string;
  sentBytes: number;
  sentPackets: number;
  droppedPackets: number;
}

/**
 * Интерфейс, смотрящий в сторону клиентов, — тот, через который уходит маршрут по умолчанию.
 *
 * Определяется каждый раз, а не сохраняется: имя может смениться при переезде на другого
 * провайдера или пересборке сети, и запомненное значение тогда молча указывало бы в никуда.
 */
export async function resolveClientFacingInterface(): Promise<string | null> {
  try {
    const { stdout } = await execFileAsync('ip', ['-o', 'route', 'show', 'default']);
    const match = /\bdev\s+(\S+)/.exec(stdout);
    return match ? match[1] : null;
  } catch (err: unknown) {
    logger.error({ err: err instanceof Error ? err.message : String(err) }, 'Failed to resolve the default-route interface');
    return null;
  }
}

/**
 * Разбирает вывод `tc -s filter show`.
 *
 * Отделено от исполнения намеренно: это единственная часть с логикой, и проверять её надо на
 * настоящем выводе ядра. Существенно, что каждый фильтр печатает ТРИ строки с одним и тем же
 * `pref`, и только после третьей идёт строка со счётчиками.
 *
 * Текст префикса приходит извне, из сохранённого состояния: в самом выводе адрес лежит
 * шестнадцатеричной маской, и восстанавливать из неё CIDR значило бы завести второй, независимый
 * способ ошибиться.
 */
export function parseFilterCounters(output: string, prefixes: string[]): ThrottleCounter[] {
  const counters: ThrottleCounter[] = [];
  let currentPref: number | null = null;

  for (const line of output.split('\n')) {
    const prefMatch = /\bpref\s+(\d+)\s+u32\b/.exec(line);
    if (prefMatch) {
      currentPref = Number(prefMatch[1]);
      continue;
    }

    const sentMatch = /\bSent\s+(\d+)\s+bytes\s+(\d+)\s+pkts?\s+\(dropped\s+(\d+)/.exec(line);
    if (!sentMatch || currentPref === null) continue;

    const prefix = prefixes[currentPref - 1];
    // Приоритет вне известного списка — не наш фильтр либо остаток от прошлой раскладки.
    // Пропускаем молча: приписать эти байты чужому префиксу было бы хуже, чем не показать их.
    if (prefix) {
      counters.push({
        prefix,
        sentBytes: Number(sentMatch[1]),
        sentPackets: Number(sentMatch[2]),
        droppedPackets: Number(sentMatch[3]),
      });
    }
    currentPref = null;
  }

  return counters;
}

async function readState(): Promise<ThrottleState | null> {
  try {
    return JSON.parse(await fs.readFile(STATE_FILE, 'utf-8')) as ThrottleState;
  } catch {
    return null;
  }
}

async function writeState(state: ThrottleState): Promise<void> {
  await fs.mkdir(path.dirname(STATE_FILE), { recursive: true });
  await fs.writeFile(STATE_FILE, `${JSON.stringify(state, null, 2)}\n`, 'utf-8');
}

/** Одинаковы ли раскладки — включая порядок, потому что от него зависит нумерация приоритетов. */
export function isSameThrottle(a: ThrottleState | null, b: ThrottleState): boolean {
  if (!a || !Array.isArray(a.entries)) return false;
  return (
    a.interface === b.interface &&
    a.entries.length === b.entries.length &&
    a.entries.every(
      (e, i) => e.prefix === b.entries[i].prefix && e.rate === b.entries[i].rate && e.burst === b.entries[i].burst
    )
  );
}

/** Приводит раскладку к каноническому виду: сортировка по префиксу. */
export function normalizeEntries(entries: ThrottleEntry[]): ThrottleEntry[] {
  // Порядок задаём мы, а не отправитель: от него зависит нумерация приоритетов, а значит и то,
  // как счётчики сопоставляются обратно с префиксами.
  return [...entries].sort((x, y) => x.prefix.localeCompare(y.prefix));
}

/** Текущие счётчики. Читаются ДО применения — пересборка фильтров их обнуляет. */
export async function readThrottleCounters(): Promise<ThrottleCounter[]> {
  const state = await readState();
  if (!state || !Array.isArray(state.entries) || state.entries.length === 0) return [];
  try {
    const { stdout } = await execFileAsync('tc', ['-s', 'filter', 'show', 'dev', state.interface, 'parent', INGRESS_HANDLE]);
    return parseFilterCounters(stdout, state.entries.map((e) => e.prefix));
  } catch (err: unknown) {
    logger.warn({ err: err instanceof Error ? err.message : String(err) }, 'Failed to read traffic-throttle counters');
    return [];
  }
}

/**
 * Приводит раскладку к заданной.
 *
 * Ничего не делает, если она совпадает с уже применённой. Это не только экономия: пересборка
 * фильтров ОБНУЛЯЕТ счётчики отброшенного, а на них держится вся оценка «отступил или продолжает
 * ломиться». Переставлять одно и то же значило бы стирать улику на каждой доставке.
 *
 * Пересборка идёт «снести все фильтры и разложить заново». На доли секунды между этими шагами
 * ограничение не действует — направление ошибки безопасное: кого-то на миг НЕ придушили, а не
 * придушили лишнего.
 */
export async function applyThrottle(
  entries: ThrottleEntry[]
): Promise<{ changed: boolean; applied: number; message: string }> {
  const iface = await resolveClientFacingInterface();
  if (!iface) {
    return { changed: false, applied: 0, message: 'No default-route interface found' };
  }

  const desired: ThrottleState = { interface: iface, entries: normalizeEntries(entries) };
  if (isSameThrottle(await readState(), desired)) {
    return { changed: false, applied: desired.entries.length, message: 'Unchanged' };
  }

  // `replace` вместо `add`: qdisc может уже стоять с прошлого раза, и `add` на нём — ошибка.
  await execFileAsync('tc', ['qdisc', 'replace', 'dev', iface, 'handle', INGRESS_HANDLE, 'ingress']);
  await execFileAsync('tc', ['filter', 'del', 'dev', iface, 'parent', INGRESS_HANDLE]).catch(() => {
    // Фильтров могло не быть вовсе — это не ошибка.
  });

  let applied = 0;
  for (const [index, entry] of desired.entries.entries()) {
    const isV6 = entry.prefix.includes(':');
    try {
      await execFileAsync('tc', [
        'filter', 'add', 'dev', iface, 'parent', INGRESS_HANDLE,
        'protocol', isV6 ? 'ipv6' : 'ip',
        'prio', String(index + 1),
        'u32', 'match', isV6 ? 'ip6' : 'ip', 'src', entry.prefix,
        'police', 'rate', entry.rate, 'burst', entry.burst, 'conform-exceed', 'drop/ok',
      ]);
      applied += 1;
    } catch (err: unknown) {
      // Один неразобранный префикс не должен отменять всю раскладку — остальные всё равно нужны.
      logger.warn({ err: err instanceof Error ? err.message : String(err), prefix: entry.prefix }, 'Failed to install a throttle filter');
    }
  }

  await writeState(desired);
  logger.info({ iface, applied }, 'Traffic throttle applied');
  return { changed: true, applied, message: 'Applied' };
}

/** Снимает ограничение целиком. Используется при выключении механизма и при удалении агента. */
export async function clearThrottle(): Promise<void> {
  const state = await readState();
  const iface = state?.interface ?? (await resolveClientFacingInterface());
  if (!iface) return;
  await execFileAsync('tc', ['qdisc', 'del', 'dev', iface, 'ingress']).catch(() => {
    // Не стоял — и хорошо.
  });
  await fs.rm(STATE_FILE, { force: true });
}
