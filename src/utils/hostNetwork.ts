import fs from 'fs/promises';
import os from 'os';

/**
 * Трафик машины для плиток оркестратора (2026-10-21): счётчики ядра с загрузки на интерфейсе
 * маршрута по умолчанию. Сумма всех интерфейсов задвоила бы мосты, veth и туннель меша — тот же
 * выбор, что у плитки машины оркестратора (`hostTelemetry.ts` там). Скорость считает оркестратор
 * разницей двух замеров: агенту для этого пришлось бы помнить прошлый.
 */
export interface HostNetwork {
  iface: string;
  rxBytes: number;
  txBytes: number;
}

/** Интерфейс маршрута по умолчанию из `/proc/net/route`: назначение 0, флаг UP, меньшая метрика. */
export function defaultRouteInterface(routeText: string): string | null {
  let best: { iface: string; metric: number } | null = null;
  for (const line of routeText.split('\n').slice(1)) {
    const cols = line.trim().split(/\s+/);
    if (cols.length < 8 || cols[1] !== '00000000') continue;
    const flags = parseInt(cols[3], 16);
    if (!(flags & 0x1)) continue;
    const metric = parseInt(cols[6], 10) || 0;
    if (!best || metric < best.metric) best = { iface: cols[0], metric };
  }
  return best?.iface ?? null;
}

/** Байты приёма и отдачи интерфейса из `/proc/net/dev`. */
export function interfaceCounters(devText: string, iface: string): { rxBytes: number; txBytes: number } | null {
  for (const line of devText.split('\n')) {
    const sep = line.indexOf(':');
    if (sep < 0 || line.slice(0, sep).trim() !== iface) continue;
    const cols = line.slice(sep + 1).trim().split(/\s+/);
    const rxBytes = Number(cols[0]);
    const txBytes = Number(cols[8]);
    return Number.isFinite(rxBytes) && Number.isFinite(txBytes) ? { rxBytes, txBytes } : null;
  }
  return null;
}

/** `null` — маршрута по умолчанию нет или `/proc` недоступен: оркестратор покажет «нет данных». */
export async function getHostNetwork(): Promise<HostNetwork | null> {
  try {
    const [route, dev] = await Promise.all([fs.readFile('/proc/net/route', 'utf-8'), fs.readFile('/proc/net/dev', 'utf-8')]);
    const iface = defaultRouteInterface(route);
    const counters = iface ? interfaceCounters(dev, iface) : null;
    return iface && counters ? { iface, ...counters } : null;
  } catch {
    return null;
  }
}

/** Поля телеметрии: пустое имя интерфейса — данных нет. */
export function hostNetworkFields(net: HostNetwork | null): { hostNetIface: string; hostNetRxBytes: number; hostNetTxBytes: number } {
  return net ? { hostNetIface: net.iface, hostNetRxBytes: net.rxBytes, hostNetTxBytes: net.txBytes } : { hostNetIface: '', hostNetRxBytes: 0, hostNetTxBytes: 0 };
}

/**
 * Глобальный unicast IPv6 (2000::/3) — всё, что видно снаружи (2026-11-21). Одним условием
 * отсекаются loopback, link-local (fe80::/10), ULA (fc00::/7 — адреса меша и Docker), multicast и
 * IPv4-mapped: перечислять исключения по одному значило бы однажды пропустить новое.
 */
export function isGlobalUnicastIpv6(address: string): boolean {
  const first = address.split('%')[0].split(':')[0];
  if (!/^[0-9a-f]{1,4}$/i.test(first)) return false;
  const value = parseInt(first, 16);
  return value >= 0x2000 && value <= 0x3fff;
}

/** Глобальные IPv6 машины по списку интерфейсов (`os.networkInterfaces()`): без повторов, по порядку. */
export function globalIpv6Addresses(interfaces: NodeJS.Dict<os.NetworkInterfaceInfo[]>): string[] {
  const out = new Set<string>();
  for (const list of Object.values(interfaces)) {
    for (const info of list ?? []) {
      if (info.family === 'IPv6' && !info.internal && isGlobalUnicastIpv6(info.address)) out.add(info.address.toLowerCase());
    }
  }
  return [...out].sort();
}

/** Поля телеметрии: `hostGlobalIpv6Reported` — агент знает поле, пустой список значит «IPv6 нет». */
export function hostIpv6Fields(interfaces: NodeJS.Dict<os.NetworkInterfaceInfo[]> = os.networkInterfaces()): { hostGlobalIpv6: string[]; hostGlobalIpv6Reported: boolean } {
  try {
    return { hostGlobalIpv6: globalIpv6Addresses(interfaces), hostGlobalIpv6Reported: true };
  } catch {
    return { hostGlobalIpv6: [], hostGlobalIpv6Reported: false };
  }
}
