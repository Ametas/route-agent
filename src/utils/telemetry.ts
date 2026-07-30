import * as fs from 'fs/promises';
import http from 'http';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync, execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

export interface CpuStats {
  idle: number;
  total: number;
}

/**
 * Читает и высчитывает дельту утилизации CPU из /proc/stat
 */
export async function getCpuUsage(prevStats: CpuStats): Promise<{ cpuUsage: number; newStats: CpuStats }> {
  try {
    const stat = await fs.readFile('/proc/stat', 'utf-8');
    const firstLine = stat.split('\n')[0];
    const times = firstLine.split(/\s+/).slice(1).map(Number);
    const total = times.reduce((a, b) => a + b, 0);
    const idle = times[3];

    const diffIdle = idle - prevStats.idle;
    const diffTotal = total - prevStats.total;
    const newStats = { idle, total };

    const cpuUsage = diffTotal === 0 ? 0 : ((diffTotal - diffIdle) / diffTotal) * 100;
    return { cpuUsage, newStats };
  } catch {
    return { cpuUsage: 0, newStats: prevStats };
  }
}

/**
 * Вычисляет процент занятой памяти на основе /proc/meminfo
 */
export async function getMemoryUsage(): Promise<number> {
  try {
    const meminfo = await fs.readFile('/proc/meminfo', 'utf-8');
    const totalMatch = meminfo.match(/MemTotal:\s+(\d+)/);
    const availMatch = meminfo.match(/MemAvailable:\s+(\d+)/);
    if (!totalMatch || !availMatch) return 0;
    
    const total = parseInt(totalMatch[1], 10);
    const avail = parseInt(availMatch[1], 10);
    return ((total - avail) / total) * 100;
  } catch {
    return 0;
  }
}

/**
 * Подсчитывает суммарное количество активных TCP/UDP сессий ноды
 */
export async function getConnectionCount(): Promise<number> {
  try {
    const tcp = await fs.readFile('/proc/net/tcp', 'utf-8');
    const udp = await fs.readFile('/proc/net/udp', 'utf-8');
    const tcpLines = tcp.trim().split('\n').length - 1;
    const udpLines = udp.trim().split('\n').length - 1;
    return Math.max(0, tcpLines + udpLines);
  } catch {
    return 0;
  }
}

/**
 * Выполняет локальный HTTP GET-запрос и возвращает распарсенный JSON
 */
function getJson(url: string, timeoutMs = 3000): Promise<any> {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      if (res.statusCode && (res.statusCode < 200 || res.statusCode >= 300)) {
        reject(new Error(`Status Code: ${res.statusCode}`));
        return;
      }
      let data = '';
      res.on('data', (chunk) => {
        data += chunk;
      });
      res.on('end', () => {
        try {
          resolve(JSON.parse(data));
        } catch (err) {
          reject(err);
        }
      });
    });

    req.on('error', (err) => {
      reject(err);
    });

    req.setTimeout(timeoutMs, () => {
      req.destroy();
      reject(new Error('Timeout'));
    });
  });
}

/**
 * Получает текущий статус WebRTC-слоя на основе опроса olcrtc-manager API
 */
export async function getWebRtcStatus(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_WEBRTC_CHECK !== 'true') {
    return 'nominal';
  }

  const port = process.env.OLCRTC_PORT ? parseInt(process.env.OLCRTC_PORT, 10) : 8888;
  const url = `http://127.0.0.1:${port}/api/state`;

  try {
    const data = await getJson(url, 1000);
    
    const runningCount = (data && typeof data.running_count === 'number') ? data.running_count : null;
    
    if (runningCount === 0) {
      let activeUsers = 0;
      if (data) {
        if (typeof data.active_users === 'number') {
          activeUsers = data.active_users;
        } else if (typeof data.users_count === 'number') {
          activeUsers = data.users_count;
        } else if (typeof data.user_count === 'number') {
          activeUsers = data.user_count;
        } else if (typeof data.online_users === 'number') {
          activeUsers = data.online_users;
        } else if (typeof data.active_connections === 'number') {
          activeUsers = data.active_connections;
        } else if (typeof data.users === 'number') {
          activeUsers = data.users;
        } else if (Array.isArray(data.users)) {
          activeUsers = data.users.length;
        }
      }
      
      if (activeUsers > 0) {
        return 'no_active_tunnels';
      }
    }
    
    return 'nominal';
  } catch (err) {
    return 'panel_dead';
  }
}

/**
 * Определение текущей версии sing-box бинарника
 */
export async function getSingBoxVersion(): Promise<string> {
  const binaryPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
  try {
    const { stdout } = await execFileAsync(binaryPath, ['version']);
    const match = stdout.match(/sing-box version ([0-9.]+)/) || stdout.match(/version\s+([\w\.\-]+)/i);
    return match ? match[1] : (stdout.split('\n')[0].trim() || 'not_installed');
  } catch {
    return 'not_installed';
  }
}

let cachedSingboxVersion = '';
let lastSingboxCheckTime = 0;

export async function getSingBoxVersionCached(): Promise<string> {
  const now = Date.now();
  if (cachedSingboxVersion && (now - lastSingboxCheckTime < 60000)) {
    return cachedSingboxVersion;
  }
  cachedSingboxVersion = await getSingBoxVersion();
  lastSingboxCheckTime = now;
  return cachedSingboxVersion;
}

export function invalidateSingboxVersionCache(): void {
  cachedSingboxVersion = '';
  lastSingboxCheckTime = 0;
}

/**
 * Опрос состояния L3-интерфейса awg0 и подсчёт числа активных пиров (хэндшейк < 3 мин назад)
 */
export async function getAwgActivePeersCount(): Promise<number> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_PEERS === undefined) {
    return 0;
  }
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_PEERS !== undefined) {
    return parseInt(process.env.TEST_AWG_PEERS, 10) || 0;
  }

  try {
    const { stdout } = await execAsync('awg show awg0 dump');
    const lines = stdout.trim().split('\n');
    if (lines.length <= 1) return 0;

    const now = Math.floor(Date.now() / 1000);
    let activePeers = 0;

    for (let i = 1; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line) continue;
      const parts = line.split(/\s+/);
      if (parts.length >= 5) {
        const latestHandshake = parseInt(parts[4], 10);
        if (!isNaN(latestHandshake) && latestHandshake > 0 && (now - latestHandshake) <= 180) {
          activePeers++;
        }
      }
    }
    return activePeers;
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.debug({ err: msg }, 'Failed to query awg0 peer status or interface not active');
    return 0;
  }
}

/**
 * Парсит семантическую версию или гибридную дату из вывода команд awg / amneziawg-go.
 * Поддерживает форматы SemVer (v3.0.2, 3.0.2), датировки WireGuard (0.0.20250522, 1.0.20260618-2) и суффиксы.
 */
export function parseAwgVersionString(stdout: string): string | null {
  if (!stdout) return null;
  const match = stdout.match(/amneziawg-go\s+v?([0-9\.]+[\w\-]*)/i) || 
                stdout.match(/amneziawg-go\s+v?([0-9\.]+)/i) ||
                stdout.match(/v?([0-9\.]+\-[0-9a-zA-Z]+)/i) ||
                stdout.match(/v?([0-9]+\.[0-9]+[0-9a-zA-Z.-]*)/i) ||
                stdout.match(/([0-9\.]+)/);
  if (match && match[1]) {
    const cleaned = match[1].replace(/^v/i, '').replace(/[\s.-]+$/, '').trim();
    if (cleaned.length > 0 && cleaned !== '.') {
      return cleaned;
    }
  }
  return null;
}

/**
 * Определение версии amneziawg-tools (утилиты awg)
 */
export async function getAwgToolsVersion(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_TOOLS_VERSION !== undefined) {
    return process.env.TEST_AWG_TOOLS_VERSION;
  }
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_VERSION !== undefined) {
    return process.env.TEST_AWG_VERSION;
  }

  const pathsToTry = Array.from(new Set([
    config.AWG_TOOLS_BINARY_PATH || '/usr/local/bin/awg',
    '/usr/local/bin/awg',
    '/usr/bin/awg'
  ]));

  for (const binPath of pathsToTry) {
    try {
      const { stdout, stderr } = await execFileAsync(binPath, ['--version']);
      const out = (stdout || stderr || '').trim();

      if (out.toLowerCase().includes('amneziawg-go')) {
        return 'not_installed';
      }

      const ver = parseAwgVersionString(out);
      if (ver) return ver;
    } catch {}
  }

  try {
    const { stdout, stderr } = await execAsync('awg --version');
    const out = (stdout || stderr || '').trim();

    if (out.toLowerCase().includes('amneziawg-go')) {
      return 'not_installed';
    }

    const ver = parseAwgVersionString(out);
    if (ver) return ver;
  } catch {}

  return 'not_installed';
}

/**
 * Определение версии Go-ядра amneziawg-go
 */
export async function getAmneziaWgGoVersion(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AMNEZIAWG_GO_VERSION !== undefined) {
    return process.env.TEST_AMNEZIAWG_GO_VERSION;
  }

  const pathsToTry = Array.from(new Set([
    config.AWG_GO_BINARY_PATH || '/usr/local/bin/amneziawg-go',
    '/usr/local/bin/amneziawg-go'
  ]));

  for (const binPath of pathsToTry) {
    try {
      const { stdout, stderr } = await execFileAsync(binPath, ['--version']);
      const out = (stdout || stderr || '').trim();

      const match = out.match(/amneziawg-go\s+v?([0-9\.]+[\w\-]*)/i) ||
                    out.match(/amneziawg-go\s+v?([0-9\.]+)/i);
      if (match && match[1]) {
        return match[1].replace(/^v/i, '').trim();
      }

      const ver = parseAwgVersionString(out);
      if (ver) return ver;
    } catch {}
  }

  return 'not_installed';
}

/**
 * Алиас getAwgVersion -> getAwgToolsVersion для обратной совместимости
 */
export async function getAwgVersion(): Promise<string> {
  return getAwgToolsVersion();
}

let cachedAwgToolsVersion = '';
let cachedAmneziaWgGoVersion = '';
let lastAwgCheckTime = 0;

export async function getAwgToolsVersionCached(): Promise<string> {
  const now = Date.now();
  if (cachedAwgToolsVersion && (now - lastAwgCheckTime < 60000)) {
    return cachedAwgToolsVersion;
  }
  cachedAwgToolsVersion = await getAwgToolsVersion();
  lastAwgCheckTime = now;
  return cachedAwgToolsVersion;
}

export async function getAmneziaWgGoVersionCached(): Promise<string> {
  const now = Date.now();
  if (cachedAmneziaWgGoVersion && (now - lastAwgCheckTime < 60000)) {
    return cachedAmneziaWgGoVersion;
  }
  cachedAmneziaWgGoVersion = await getAmneziaWgGoVersion();
  lastAwgCheckTime = now;
  return cachedAmneziaWgGoVersion;
}

export async function getAwgVersionCached(): Promise<string> {
  return getAwgToolsVersionCached();
}

export function invalidateAwgVersionCache(): void {
  cachedAwgToolsVersion = '';
  cachedAmneziaWgGoVersion = '';
  lastAwgCheckTime = 0;
}

export async function getAwgStatus(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_STATUS !== undefined) {
    return process.env.TEST_AWG_STATUS;
  }
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_STATUS === undefined) {
    return 'nominal';
  }
  try {
    await execAsync('ip link show awg0');
    return 'nominal';
  } catch {
    return 'inactive';
  }
}


