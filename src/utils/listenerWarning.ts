import * as fs from 'fs/promises';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync } from './exec.js';
import type { JournalWarning } from './journalWarnings.js';

const logger = pino({ level: 'info' });

/**
 * «Ядро работает, но никого не обслуживает» — проверка, которой не хватило три недели.
 *
 * ЧТО СЛУЧИЛОСЬ (нода mo-nl-node, 2026-09-04). Фронтовой sing-box числился живым и отдавал версию,
 * карточка узла выглядела здоровой, телеметрия шла. А на публичном порту не слушал НИКТО: процесс
 * держал в памяти конфиг трёхнедельной давности, потому что каждый `reload` падал, а на диске лежала
 * заглушка `{"route":{"rules":[]}}`. Снаружи это выглядело как таймауты hysteria2 и tuic при живом
 * VLESS — тот терминирует Caddy, и он перезагружался отдельно и успешно.
 *
 * Ни одна существующая проверка этого не видела: версия — про файл, живость — про службу, журнал —
 * про то, что процесс СКАЗАЛ. А он ничего не говорил, он просто ничего не слушал.
 *
 * ПОЧЕМУ ЭТО ЖИВЁТ ЗДЕСЬ, А НЕ В ЖУРНАЛЬНЫХ ОБРАЗЦАХ. Класс проблемы тот же — тихая деградация, —
 * и уезжает находка тем же RPC `PullNodeWarnings`. Но источник другой: не строка в журнале, а
 * состояние сокетов. Мешать их в одном реестре образцов значило бы притвориться, что грепом можно
 * найти отсутствие события.
 */

/** Стабильный идентификатор класса — по нему оркестратор дедуплицирует доклады. */
export const CORE_NOT_LISTENING_KIND = 'core_not_listening';

/**
 * Порты, которые конфиг объявляет ПУБЛИЧНЫМИ.
 *
 * Инбаунд на loopback не считается: тыловой инстанс и диспетчер слушают 127.0.0.1 совершенно
 * законно, и требовать их наличия снаружи было бы ложной тревогой.
 */
export function publicListenPorts(configObj: unknown): number[] {
  if (typeof configObj !== 'object' || configObj === null) return [];
  const inbounds = (configObj as { inbounds?: unknown }).inbounds;
  if (!Array.isArray(inbounds)) return [];

  const ports: number[] = [];
  for (const raw of inbounds) {
    if (typeof raw !== 'object' || raw === null) continue;
    const inbound = raw as { listen?: unknown; listen_port?: unknown };

    const listen = typeof inbound.listen === 'string' ? inbound.listen : '';
    // Пусто означает «все интерфейсы» — это тоже публичный слушатель.
    if (listen && listen !== '0.0.0.0' && listen !== '::') continue;

    const port = typeof inbound.listen_port === 'number' ? inbound.listen_port : NaN;
    if (Number.isInteger(port) && port > 0 && !ports.includes(port)) ports.push(port);
  }

  return ports;
}

/** Порты, на которых кто-нибудь действительно слушает — по выводу `ss`. */
export function parseListeningPorts(ssOutput: string): number[] {
  const ports: number[] = [];
  for (const line of ssOutput.split('\n')) {
    // Локальный адрес — предпоследнее поле перед peer'ом; берём хвост после последнего двоеточия.
    const match = line.match(/[\s%][^\s]*:(\d+)\s+[^\s]+:\*/);
    if (!match) continue;
    const port = Number(match[1]);
    if (Number.isInteger(port) && !ports.includes(port)) ports.push(port);
  }
  return ports;
}

/**
 * Возвращает предупреждение, если ядро активно, конфиг объявляет публичные порты, а слушателей на
 * них нет.
 *
 * МОЛЧИТ ВО ВСЕХ СОМНИТЕЛЬНЫХ СЛУЧАЯХ, и это осознанно: ядро не установлено, конфига нет, конфиг без
 * публичных инбаундов, `ss` недоступен — везде возвращается `null`. Ложная тревога здесь дороже
 * пропуска: она приучает не читать доклады, а именно на них и держится вся затея.
 */
export async function detectCoreNotListening(
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync
): Promise<JournalWarning | null> {
  const configPath = config.SINGBOX_CONFIG_PATH || '/etc/sing-box/config.json';

  const rawConfig = await fs.readFile(configPath, 'utf-8').catch(() => null);
  if (!rawConfig) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(rawConfig);
  } catch {
    return null;
  }

  const expected = publicListenPorts(parsed);
  if (expected.length === 0) return null;

  let listening: number[];
  try {
    const [udp, tcp] = await Promise.all([runExec('ss -lnu'), runExec('ss -lnt')]);
    listening = [...parseListeningPorts(udp.stdout), ...parseListeningPorts(tcp.stdout)];
  } catch (err: any) {
    logger.debug({ err: err?.message }, 'Listener check skipped: ss is unavailable');
    return null;
  }

  const missing = expected.filter((port) => !listening.includes(port));
  if (missing.length === 0) return null;

  return {
    kind: CORE_NOT_LISTENING_KIND,
    source: 'sing-box',
    sample: `конфиг объявляет порты ${expected.join(', ')}, но никто не слушает ${missing.join(', ')}`,
    occurrences: missing.length,
    lastSeenUnixMs: Date.now(),
  };
}
