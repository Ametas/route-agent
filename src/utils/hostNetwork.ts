import fs from 'fs/promises';

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
