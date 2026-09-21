import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import { execAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Абсолютный путь к локальному файлу-кэшу, хранящему список UDP-портов,
 * которые были открыты в UFW при прошлых настройках.
 */
const ACTIVE_PORTS_CACHE_PATH = '/opt/route-agent/active_ports.json';

/**
 * Множество типов sing-box inbound'ов, которые работают поверх UDP
 * и требуют динамического управления правилами файрвола.
 */
const UDP_TUNNEL_INBOUND_TYPES = new Set(['hysteria2', 'tuic']);

/**
 * Проверяет, установлен ли в системе ufw
 */
export async function isUfwInstalled(): Promise<boolean> {
  try {
    await execAsync('command -v ufw');
    return true;
  } catch {
    return false;
  }
}

/**
 * Читает список ранее открытых портов из локального файла-кэша
 */
export async function readActivePortsCache(): Promise<number[]> {
  try {
    const raw = await fs.readFile(ACTIVE_PORTS_CACHE_PATH, 'utf-8');
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((p: unknown): p is number => typeof p === 'number') : [];
  } catch {
    return [];
  }
}

/**
 * Сохраняет актуальный список открытых портов в локальный файл-кэш
 */
export async function writeActivePortsCache(ports: number[]): Promise<void> {
  await fs.mkdir(path.dirname(ACTIVE_PORTS_CACHE_PATH), { recursive: true });
  await fs.writeFile(ACTIVE_PORTS_CACHE_PATH, JSON.stringify(ports, null, 2), 'utf-8');
}

/**
 * Извлекает из объекта конфигурации sing-box уникальные порты входящих
 * соединений типов hysteria2/tuic, работающих поверх UDP
 */
export function extractUdpTunnelPorts(configObj: Record<string, unknown>): number[] {
  const inbounds = Array.isArray(configObj?.inbounds) ? (configObj.inbounds as Record<string, unknown>[]) : [];
  const ports = new Set<number>();

  for (const inbound of inbounds) {
    if (!inbound || typeof inbound !== 'object') continue;
    if (typeof inbound.type === 'string' && !UDP_TUNNEL_INBOUND_TYPES.has(inbound.type)) continue;

    const rawPort = inbound.listen_port ?? inbound.port;
    const port = Number(rawPort);
    if (Number.isInteger(port) && port > 0 && port <= 65535) {
      ports.add(port);
    }
  }

  return Array.from(ports);
}

export interface FirewallSyncPlan {
  toOpen: number[];
  toClose: number[];
  /** Нужна ли перезагрузка правил. Ровно тогда, когда есть что применять. */
  needsReload: boolean;
}

/**
 * Что делать с фаерволом — чистое решение, отдельно от его исполнения.
 *
 * ⚠️ `needsReload` ЗАВИСИТ ОТ НАЛИЧИЯ ИЗМЕНЕНИЙ, и до аудита 2026-09-21 не зависел: `ufw reload`
 * выполнялся безусловно, в конце каждой синхронизации. А синхронизация идёт на КАЖДОМ применении
 * конфига — то есть на каждом создании абонента флот перестраивал правила iptables, хотя портовый
 * состав от появления абонента не меняется никогда.
 *
 * Вынесено в чистую функцию, чтобы это правило можно было проверить тестом: сам `syncEgressFirewall`
 * ходит в `sudo ufw` и в файл по абсолютному пути, и проверяем он только на живой машине.
 */
export function planFirewallSync(newPorts: number[], previousPorts: number[]): FirewallSyncPlan {
  const newPortsSet = new Set(newPorts);
  const previousPortsSet = new Set(previousPorts);

  const toOpen = newPorts.filter((port) => !previousPortsSet.has(port));
  const toClose = previousPorts.filter((port) => !newPortsSet.has(port));

  return { toOpen, toClose, needsReload: toOpen.length > 0 || toClose.length > 0 };
}

/**
 * Синхронизирует правила UFW с актуальным списком UDP-портов hysteria2/tuic
 */
export async function syncEgressFirewall(configObj: Record<string, unknown>): Promise<void> {
  if (!(await isUfwInstalled())) {
    logger.warn('ufw is not installed on this system; skipping egress firewall synchronization');
    return;
  }

  try {
    const newPorts = extractUdpTunnelPorts(configObj);
    const previousPorts = await readActivePortsCache();

    const { toOpen: portsToOpen, toClose: portsToClose, needsReload } = planFirewallSync(newPorts, previousPorts);

    if (!needsReload) {
      // Портовый состав не изменился — применять нечего, и перезагружать правила незачем.
      logger.debug({ ports: newPorts.length }, 'Egress firewall already in sync — skipping UFW reload');
      return;
    }

    await Promise.allSettled(
      portsToOpen.map(async (port) => {
        try {
          const { stdout, stderr } = await execAsync(`sudo ufw allow ${port}/udp`);
          logger.info({ port, stdout, stderr }, 'Opened UDP firewall port for egress tunnel inbound');
        } catch (err: unknown) {
          const stderr = (err as { stderr?: string; message?: string }).stderr || (err as Error).message;
          logger.error({ port, err: stderr }, 'Failed to open UFW UDP port');
        }
      })
    );

    await Promise.allSettled(
      portsToClose.map(async (port) => {
        try {
          const { stdout, stderr } = await execAsync(`sudo ufw delete allow ${port}/udp`);
          logger.info({ port, stdout, stderr }, 'Closed stale UDP firewall port no longer used by egress config');
        } catch (err: unknown) {
          const stderr = (err as { stderr?: string; message?: string }).stderr || (err as Error).message;
          logger.error({ port, err: stderr }, 'Failed to close UFW UDP port');
        }
      })
    );

    try {
      const { stdout, stderr } = await execAsync('sudo ufw reload');
      if (stdout) logger.info({ stdout }, 'UFW reload stdout');
      if (stderr) logger.warn({ stderr }, 'UFW reload stderr');
    } catch (err: unknown) {
      const stderr = (err as { stderr?: string; message?: string }).stderr || (err as Error).message;
      logger.error({ err: stderr }, 'Failed to reload UFW after egress firewall synchronization');
    }

    await writeActivePortsCache(newPorts);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Unexpected error while synchronizing egress firewall state');
  }
}
