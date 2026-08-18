import * as fs from 'fs/promises';
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
 * Получает текущий статус WebRTC-слоя (olcrtc-agent-srv) — план `olcrtc-redesign.md`, "Новая
 * архитектура: собственный слой". Заменяет старый опрос HTTP `/api/state` стороннего
 * olcrtc-manager (которого больше нет) на прямую проверку бинаря + подсчёт реально запущенных
 * per-instance systemd-юнитов (olcrtc-agent-srv@*), той же "exec+parse, live" философией, что
 * singbox_version/awg_status. Детальная событийная телеметрия (сессии/трафик) идёт отдельным
 * каналом — StreamOlcrtcEvents (olcrtc.service.ts), это поле остаётся грубым health-индикатором.
 */
export async function getWebRtcStatus(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_WEBRTC_CHECK !== 'true') {
    return 'nominal';
  }

  const binPath = config.OLCRTC_AGENT_SRV_BINARY_PATH;
  const binExists = await fs.stat(binPath).then(() => true).catch(() => false);
  if (!binExists) {
    return 'not_installed';
  }

  try {
    const { stdout } = await execAsync(
      "systemctl list-units --type=service --state=running --no-legend 'olcrtc-agent-srv@*' | awk '{print $1}'"
    );
    const runningCount = stdout.split('\n').map((l) => l.trim()).filter(Boolean).length;
    return runningCount === 0 ? 'no_active_tunnels' : 'nominal';
  } catch (err) {
    return 'unavailable';
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

import { getAwgInterfaceName, getMeshAwgInterfaceName } from './awg.js';

// --- S2S AWG3 mesh tunnel (front↔egress, second ring-relay hop) telemetry ---------------------
// Deliberately NOT wired into checkAndHealAwgInterface's auto-restart loop below — this is our
// own server-to-server infra tunnel, not a client-facing service; silently self-healing it could
// mask a real connectivity problem an admin needs to see, unlike the client AWG case where
// keeping paying users connected is the priority. Read-only status detection only.

/**
 * modinfo reads the on-disk/depmod module regardless of load state (distinct signal from lsmod),
 * so "built_not_loaded" (module present but not currently active — e.g. right after a reboot
 * before systemd-modules-load ran) is distinguishable from "not_installed" (DKMS never
 * built/installed it at all). No proto change needed for either extra value — the wire field is a
 * free string, not an enum.
 */
export async function getMeshAwgKernelStatus(): Promise<{ status: string; version: string }> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_MESH_AWG_KERNEL_STATUS !== undefined) {
    return { status: process.env.TEST_MESH_AWG_KERNEL_STATUS, version: process.env.TEST_MESH_AWG_KERNEL_VERSION || '' };
  }

  let builtVersion = '';
  try {
    const { stdout } = await execAsync('modinfo amneziawg');
    const match = stdout.match(/^version:\s*(\S+)/m);
    builtVersion = match ? match[1] : '';
  } catch {
    return { status: 'not_installed', version: '' };
  }

  try {
    await execAsync('lsmod | grep -w amneziawg');
    return { status: 'loaded', version: builtVersion };
  } catch {
    return { status: 'built_not_loaded', version: builtVersion };
  }
}

let cachedMeshAwgKernelStatus: { status: string; version: string } | null = null;
let lastMeshAwgKernelCheckTime = 0;

export async function getMeshAwgKernelStatusCached(): Promise<{ status: string; version: string }> {
  const now = Date.now();
  if (cachedMeshAwgKernelStatus && (now - lastMeshAwgKernelCheckTime < 60000)) {
    return cachedMeshAwgKernelStatus;
  }
  cachedMeshAwgKernelStatus = await getMeshAwgKernelStatus();
  lastMeshAwgKernelCheckTime = now;
  return cachedMeshAwgKernelStatus;
}

export function invalidateMeshAwgKernelStatusCache(): void {
  cachedMeshAwgKernelStatus = null;
  lastMeshAwgKernelCheckTime = 0;
}

/**
 * "absent" (interface doesn't exist at all) vs "up" (interface exists — kernel-module-backed
 * AWG interfaces don't have a meaningful "down but configured" state the way systemd units do,
 * since awg-quick down removes the interface entirely) — mirrors getAwgActivePeersCount's
 * handshake-recency window (180s) for the active-peer count.
 */
export async function getMeshTunnelInterfaceStatus(): Promise<{ status: string; activePeers: number }> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_MESH_TUNNEL_STATUS !== undefined) {
    return {
      status: process.env.TEST_MESH_TUNNEL_STATUS,
      activePeers: parseInt(process.env.TEST_MESH_TUNNEL_ACTIVE_PEERS || '0', 10) || 0,
    };
  }

  const iface = getMeshAwgInterfaceName();
  try {
    await execAsync(`ip link show ${iface}`);
  } catch {
    return { status: 'absent', activePeers: 0 };
  }

  try {
    const { stdout } = await execAsync(`awg show ${iface} dump`);
    const lines = stdout.trim().split('\n');
    if (lines.length <= 1) return { status: 'up', activePeers: 0 };

    const now = Math.floor(Date.now() / 1000);
    let activePeers = 0;
    for (let i = 1; i < lines.length; i++) {
      const parts = lines[i].trim().split(/\s+/);
      if (parts.length >= 5) {
        const latestHandshake = parseInt(parts[4], 10);
        if (!isNaN(latestHandshake) && latestHandshake > 0 && (now - latestHandshake) <= 180) {
          activePeers++;
        }
      }
    }
    return { status: 'up', activePeers };
  } catch {
    return { status: 'up', activePeers: 0 };
  }
}

export interface AwgHealthState {
  lastCheckTime: number;
  lastAutoRestartTime: number;
  autoRestartCount: number;
  status: string;
}

const awgHealthStates: Record<string, AwgHealthState> = {};

/**
 * Проверяет состояние AWG-интерфейса и выполняет автоматический рестарт (self-healing) при падении.
 * Ограничивает частоту авто-рестартов (не чаще 1 раза в 5 минут).
 */
export async function checkAndHealAwgInterface(iface: string = getAwgInterfaceName()): Promise<AwgHealthState> {
  const now = Date.now();
  const currentState: AwgHealthState = awgHealthStates[iface] || {
    lastCheckTime: 0,
    lastAutoRestartTime: 0,
    autoRestartCount: 0,
    status: 'unknown'
  };

  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_STATUS !== undefined) {
    currentState.status = process.env.TEST_AWG_STATUS;
    currentState.lastCheckTime = now;
    awgHealthStates[iface] = currentState;
    return currentState;
  }

  // 1. Проверяем, был ли сконфигурирован/включен юнит route-awg@<iface> или конфиг
  const awgConfigPath = config.AWG_CONFIG_PATH || `/etc/amnezia/amneziawg/${iface}.conf`;
  const unitName = `route-awg@${iface}`;

  let isConfigured = false;
  if (process.env.NODE_ENV !== 'test') {
    try {
      const configExists = await fs.stat(awgConfigPath).then(() => true).catch(() => false);
      let isEnabled = false;
      try {
        const { stdout } = await execAsync(`systemctl is-enabled ${unitName}`);
        isEnabled = stdout.trim() === 'enabled';
      } catch {}
      isConfigured = configExists || isEnabled;
    } catch {
      isConfigured = false;
    }
  } else {
    isConfigured = process.env.TEST_AWG_UNCONFIGURED !== 'true';
  }

  if (!isConfigured) {
    currentState.status = 'inactive';
    currentState.lastCheckTime = now;
    awgHealthStates[iface] = currentState;
    return currentState;
  }

  // 2. Проверяем, жив ли демон / сетевой интерфейс
  let isAlive = false;
  if (process.env.NODE_ENV !== 'test') {
    try {
      await execAsync(`ip link show ${iface}`);
      isAlive = true;
    } catch {
      isAlive = false;
    }
  } else {
    isAlive = process.env.TEST_AWG_ALIVE !== 'false';
  }

  const RESTART_COOLDOWN_MS = 5 * 60 * 1000; // 5 минут ограничение частоты авто-рестартов

  if (!isAlive) {
    logger.warn({ iface }, `AWG interface ${iface} is down but unit is configured. Checking auto-restart cooldown...`);

    const timeSinceLastRestart = now - currentState.lastAutoRestartTime;
    if (currentState.lastAutoRestartTime === 0 || timeSinceLastRestart >= RESTART_COOLDOWN_MS) {
      logger.warn({ iface }, `Initiating self-healing restart for AWG service ${unitName}...`);
      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync(`systemctl restart ${unitName}`);
          currentState.lastAutoRestartTime = now;
          currentState.autoRestartCount++;
          logger.info({ iface, count: currentState.autoRestartCount }, `Self-healing restart executed for ${unitName}`);
        } catch (err: any) {
          logger.error({ iface, err: err.message }, `Self-healing restart failed for ${unitName}`);
        }
      } else {
        currentState.lastAutoRestartTime = now;
        currentState.autoRestartCount++;
      }
    } else {
      const waitSec = Math.ceil((RESTART_COOLDOWN_MS - timeSinceLastRestart) / 1000);
      logger.warn({ iface, waitSec }, `Auto-restart cooldown active for ${iface}. Next attempt allowed in ${waitSec}s.`);
    }
  }

  // 3. Формируем текстовое описание состояния с учётом последнего авто-рестарта
  let restartInfo = '';
  if (currentState.lastAutoRestartTime > 0) {
    const elapsedMs = now - currentState.lastAutoRestartTime;
    const elapsedMins = Math.floor(elapsedMs / 60000);
    if (elapsedMins < 60) {
      restartInfo = `restarted ${Math.max(0, elapsedMins)}m ago`;
    } else {
      const elapsedHours = Math.floor(elapsedMins / 60);
      restartInfo = `restarted ${elapsedHours}h ago`;
    }
  }

  if (isAlive) {
    if (restartInfo && (now - currentState.lastAutoRestartTime) < 24 * 60 * 60 * 1000) {
      currentState.status = `nominal (${restartInfo})`;
    } else {
      currentState.status = 'nominal';
    }
  } else {
    if (restartInfo) {
      currentState.status = `crashed (${restartInfo})`;
    } else {
      currentState.status = 'crashed';
    }
  }

  currentState.lastCheckTime = now;
  awgHealthStates[iface] = currentState;
  return currentState;
}

export function getAwgHealthState(iface: string = getAwgInterfaceName()): AwgHealthState {
  return awgHealthStates[iface] || {
    lastCheckTime: 0,
    lastAutoRestartTime: 0,
    autoRestartCount: 0,
    status: 'inactive'
  };
}

let awgHealthCheckTimer: NodeJS.Timeout | null = null;

export function startAwgHealthCheckTimer(intervalMs = 30000): void {
  if (awgHealthCheckTimer) return;
  if (process.env.NODE_ENV === 'test') return;

  awgHealthCheckTimer = setInterval(async () => {
    try {
      await checkAndHealAwgInterface();
    } catch (err: any) {
      logger.debug({ err: err.message }, 'Error in AWG health check timer tick');
    }
  }, intervalMs);
}

export function stopAwgHealthCheckTimer(): void {
  if (awgHealthCheckTimer) {
    clearInterval(awgHealthCheckTimer);
    awgHealthCheckTimer = null;
  }
}

export async function getAwgStatus(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_AWG_STATUS !== undefined) {
    return process.env.TEST_AWG_STATUS;
  }
  const state = await checkAndHealAwgInterface();
  return state.status;
}
