import { 
  Server, 
  ServerCredentials, 
  ServerUnaryCall, 
  sendUnaryData, 
  ServerWritableStream,
  ServerReadableStream,
  UntypedServiceImplementation,
  loadPackageDefinition
} from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { exec, execFile, spawn } from 'child_process';
import { promisify } from 'util';
import * as fs from 'fs/promises';
import { createWriteStream } from 'fs';
import path from 'path';
import pino from 'pino';
import net from 'net';
import http from 'http';
import { config } from './config.js';

const execAsync = promisify(exec);
const execFileAsync = promisify(execFile);
const logger = pino({ level: 'info' });

const PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');
const packageDefinition = protoLoader.loadSync(PROTO_PATH, {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: true,
  oneofs: true,
});

const protoDescriptor = loadPackageDefinition(packageDefinition) as any;
const agentPackage = protoDescriptor.agent;

interface CpuStats {
  idle: number;
  total: number;
}

/**
 * Читает и высчитывает дельту утилизации CPU из /proc/stat
 */
async function getCpuUsage(prevStats: CpuStats): Promise<{ cpuUsage: number; newStats: CpuStats }> {
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
async function getMemoryUsage(): Promise<number> {
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
async function getConnectionCount(): Promise<number> {
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
async function getWebRtcStatus(): Promise<string> {
  if (process.env.NODE_ENV === 'test' && process.env.TEST_WEBRTC_CHECK !== 'true') {
    return 'nominal';
  }

  const port = process.env.OLCRTC_PORT ? parseInt(process.env.OLCRTC_PORT, 10) : 8888;
  const url = `http://127.0.0.1:${port}/api/state`;

  try {
    const data = await getJson(url, 1000);
    
    // Парсим running_count
    const runningCount = (data && typeof data.running_count === 'number') ? data.running_count : null;
    
    if (runningCount === 0) {
      // Ищем количество активных пользователей в ответе (поддерживаем разные варианты ключей)
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
    // Если ответа нет или произошла ошибка запроса/парсинга -> панель упала
    return 'panel_dead';
  }
}

/**
 * Определение текущей версии sing-box бинарника
 */
async function getSingBoxVersion(): Promise<string> {
  const binaryPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
  try {
    const { stdout } = await execAsync(`${binaryPath} version`);
    const match = stdout.match(/sing-box version ([0-9.]+)/) || stdout.match(/version\s+([\w\.\-]+)/i);
    return match ? match[1] : (stdout.split('\n')[0].trim() || 'not_installed');
  } catch {
    return 'not_installed';
  }
}

let cachedSingboxVersion = '';
let lastSingboxCheckTime = 0;

async function getSingBoxVersionCached(): Promise<string> {
  const now = Date.now();
  if (cachedSingboxVersion && (now - lastSingboxCheckTime < 60000)) {
    return cachedSingboxVersion;
  }
  cachedSingboxVersion = await getSingBoxVersion();
  lastSingboxCheckTime = now;
  return cachedSingboxVersion;
}

/**
 * Опрос состояния L3-интерфейса awg0 и подсчёт числа активных пиров (хэндшейк < 3 мин назад)
 */
async function getAwgActivePeersCount(): Promise<number> {
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
 * Гарантирует права на чтение сертификатов Caddy для ядра sing-box
 */
async function fixCaddyPermissions(): Promise<void> {
  try {
    const caddyDirExists = await fs.stat('/var/lib/caddy').then(() => true).catch(() => false);
    if (caddyDirExists) {
      await execAsync('chmod -R 755 /var/lib/caddy || true');
    }
  } catch (err: any) {
    logger.warn({ err: err.message }, 'Failed to adjust Caddy certificates permissions');
  }
}

/**
 * Вспомогательный метод локальной валидации синтаксиса sing-box перед его применением
 */
async function validateSingBoxConfig(configObj: object): Promise<{ valid: boolean; error?: string }> {
  if (process.env.NODE_ENV === 'test') {
    return { valid: true };
  }
  await fixCaddyPermissions();
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const checkFilePath = path.join(targetDir, `.config.check_${Date.now()}.json`);
  const binaryPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
  
  try {
    await fs.mkdir(targetDir, { recursive: true });
    await fs.writeFile(checkFilePath, JSON.stringify(configObj, null, 2), 'utf-8');
    
    // Выполняем нативный тест синтаксиса sing-box
    await execAsync(`${binaryPath} check -c ${checkFilePath}`);
    return { valid: true };
  } catch (err: any) {
    logger.error({ stderr: err.stderr }, 'Sing-box configuration syntax check failed');
    return { valid: false, error: err.stderr || err.message };
  } finally {
    await fs.unlink(checkFilePath).catch(() => {});
  }
}

/**
 * Исполнитель применения конфигурации и мягкой перезагрузки ядра
 */
async function atomicApplyAndReload(configObj: object): Promise<void> {
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const tempFilePath = path.join(targetDir, `.config.${Date.now()}.tmp`);

  // Атомарная подмена через временный файл
  await fs.writeFile(tempFilePath, JSON.stringify(configObj, null, 2), 'utf-8');
  await fs.rename(tempFilePath, config.SINGBOX_CONFIG_PATH);

  // Мягкий reload сервиса
  if (process.env.NODE_ENV !== 'test' || process.env.RELOAD_COMMAND) {
    const { stdout, stderr } = await execAsync(config.RELOAD_COMMAND);
    if (stdout) logger.info({ stdout }, 'Reload command stdout');
    if (stderr) logger.warn({ stderr }, 'Reload command stderr');
  }
}

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
async function isUfwInstalled(): Promise<boolean> {
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
async function readActivePortsCache(): Promise<number[]> {
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
async function writeActivePortsCache(ports: number[]): Promise<void> {
  await fs.mkdir(path.dirname(ACTIVE_PORTS_CACHE_PATH), { recursive: true });
  await fs.writeFile(ACTIVE_PORTS_CACHE_PATH, JSON.stringify(ports, null, 2), 'utf-8');
}

/**
 * Извлекает из объекта конфигурации sing-box уникальные порты входящих
 * соединений типов hysteria2/tuic, работающих поверх UDP
 */
/**
 * Извлекает из объекта конфигурации sing-box уникальные порты входящих
 * соединений типов hysteria2/tuic, работающих поверх UDP
 */
function extractUdpTunnelPorts(configObj: Record<string, unknown>): number[] {
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

/**
 * Синхронизирует правила UFW с актуальным списком UDP-портов hysteria2/tuic
 */
async function syncEgressFirewall(configObj: Record<string, unknown>): Promise<void> {
  if (!(await isUfwInstalled())) {
    logger.warn('ufw is not installed on this system; skipping egress firewall synchronization');
    return;
  }

  try {
    const newPorts = extractUdpTunnelPorts(configObj);
    const previousPorts = await readActivePortsCache();

    const newPortsSet = new Set(newPorts);
    const previousPortsSet = new Set(previousPorts);

    const portsToOpen = newPorts.filter((port) => !previousPortsSet.has(port));
    const portsToClose = previousPorts.filter((port) => !newPortsSet.has(port));

    for (const port of portsToOpen) {
      try {
        const { stdout, stderr } = await execAsync(`sudo ufw allow ${port}/udp`);
        logger.info({ port, stdout, stderr }, 'Opened UDP firewall port for egress tunnel inbound');
      } catch (err: unknown) {
        const stderr = (err as { stderr?: string; message?: string }).stderr || (err as Error).message;
        logger.error({ port, err: stderr }, 'Failed to open UFW UDP port');
      }
    }

    for (const port of portsToClose) {
      try {
        const { stdout, stderr } = await execAsync(`sudo ufw delete allow ${port}/udp`);
        logger.info({ port, stdout, stderr }, 'Closed stale UDP firewall port no longer used by egress config');
      } catch (err: unknown) {
        const stderr = (err as { stderr?: string; message?: string }).stderr || (err as Error).message;
        logger.error({ port, err: stderr }, 'Failed to close UFW UDP port');
      }
    }

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

/**
 * Извлечение секрета из gRPC metadata
 */
function extractSecretFromMetadata(call: { metadata?: { get(key: string): unknown[] } }): string {
  const metadataValues = call.metadata ? call.metadata.get('x-orchestrator-secret') : [];
  return metadataValues && metadataValues[0] ? String(metadataValues[0]) : '';
}

interface ApplyConfigRequest {
  configJson: string;
}

interface ApplyConfigResponse {
  success: boolean;
  message: string;
}

interface TelemetryRequest {
  orchestratorSecret: string;
}

interface TelemetryResponse {
  cpuUsage: number;
  memUsage: number;
  activeConnections: number;
  systemLogs: string;
  timestamp: number;
  webrtcStatus: string;
  singboxVersion: string;
  awgActivePeers: number;
}

interface BinaryChunkPayload {
  orchestratorSecret?: string;
  chunk?: Buffer | Uint8Array;
  version?: string;
  isFinal?: boolean;
  targetBinary?: string;
  orchestrator_secret?: string;
  is_final?: boolean;
  target_binary?: string;
}
type BinaryChunk = BinaryChunkPayload;

interface UploadBinaryResponse {
  success: boolean;
  message: string;
}
type UpgradeResponse = UploadBinaryResponse;

interface UpgradePayload {
  version?: string;
  downloadUrl?: string;
  download_url?: string;
}

interface CaddyConfigPayload {
  domains?: string[];
  camouflageHtml?: string;
  camouflagePath?: string;
  xhttpRegexp?: string;
  xhttpRewrite?: string;
  xhttpSocket?: string;
  grpcRegexp?: string;
  grpcRewrite?: string;
  grpcSocket?: string;

  camouflage_html?: string;
  camouflage_path?: string;
  xhttp_regexp?: string;
  xhttp_rewrite?: string;
  xhttp_socket?: string;
  grpc_regexp?: string;
  grpc_rewrite?: string;
  grpc_socket?: string;

  domain?: string;
  decoyPort?: number;
  htmlContent?: string;
}

interface CaddyConfigResponse {
  success: boolean;
  message: string;
}

interface OlcrtcConfigPayload {
  enabled: boolean;
  user: string;
  password: string;
  port: number;
}

interface OlcrtcConfigResponse {
  success: boolean;
  message: string;
}

interface AwgPeer {
  publicKey: string;
  presharedKey?: string;
  allowedIps: string;
}

interface AwgConfigPayload {
  enabled: boolean;
  port: number;
  serverPrivateKey: string;
  serverPublicKey: string;
  addressV4: string;
  addressV6: string;

  // AWG3 параметры обфускации
  jc: number;
  jmin: number;
  jmax: number;
  s1: number;
  s2: number;
  s3: number;
  s4: number;
  h1: number;
  h2: number;
  h3: number;
  h4: number;
  headerProtectionKey: string;

  peers: AwgPeer[];
  ipv6Mode: string;
}

interface AwgConfigResponse {
  success: boolean;
  message: string;
}

interface FirewallPayload {
  openUdpPorts: number[];
  openTcpPorts: number[];
}

interface FirewallResponse {
  success: boolean;
  message: string;
}

interface SelfUpdatePayload {}

interface SelfUpdateResponse {
  success: boolean;
  message: string;
}

/**
 * RPC Обработчик метода ApplyConfig
 */
async function applyConfigHandler(
  call: ServerUnaryCall<ApplyConfigRequest, ApplyConfigResponse>, 
  callback: sendUnaryData<ApplyConfigResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);

  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized gRPC execution blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  try {
    const configObj = JSON.parse(call.request.configJson);
    const syntaxCheck = await validateSingBoxConfig(configObj);
    
    if (!syntaxCheck.valid) {
      return callback(null, { 
        success: false, 
        message: `Rejected by Node Agent: Invalid sing-box syntax. Error: ${syntaxCheck.error}` 
      });
    }

    await syncEgressFirewall(configObj);
    await atomicApplyAndReload(configObj);
    return callback(null, {
      success: true,
      message: 'Configuration successfully validated, applied, and sing-box reloaded via gRPC channel.'
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to process ApplyConfig RPC pipeline');
    return callback(null, { success: false, message: `Internal Agent Error: ${msg}` });
  }
}

/**
 * RPC Обработчик серверного стрима телеметрии
 */
async function streamTelemetryHandler(
  call: ServerWritableStream<TelemetryRequest, TelemetryResponse>
): Promise<void> {
  const secretHeader = call.request.orchestratorSecret;

  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized gRPC telemetry stream requested');
    call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
    return;
  }

  logger.info('Dynamic telemetry binary stream opened by orchestrator');
  let logBuffer = '';

  const journalProcess = spawn('journalctl', ['-u', 'sing-box', '-n', '10', '-f', '--output', 'cat']);
  journalProcess.on('error', (err: any) => {
    logger.warn({ err: err.message }, 'Failed to spawn journalctl process');
  });
  if (journalProcess.stdout) {
    journalProcess.stdout.on('data', (chunk: Buffer) => {
      logBuffer += chunk.toString();
      if (logBuffer.length > 8192) {
        logBuffer = logBuffer.slice(-8192); // Ограничиваем буфер последними 8 Кб
      }
    });
  }

  let telemetryInterval: NodeJS.Timeout | null = null;
  let isCleanedUp = false;
  let streamCpuStats: CpuStats = { idle: 0, total: 0 };

  // Единая функция очистки всех ресурсов стрима
  const cleanup = () => {
    if (isCleanedUp) return;
    isCleanedUp = true;

    if (telemetryInterval) {
      clearInterval(telemetryInterval);
      telemetryInterval = null;
    }
    try {
      journalProcess.kill();
    } catch {}
    logger.info('Telemetry binary stream closed and resources safely released');
  };

  // Навешиваем очистку на ВСЕ события закрытия сокета/стрима
  call.on('cancelled', cleanup);
  call.on('close', cleanup);
  call.on('finish', cleanup);
  call.on('error', cleanup);

  telemetryInterval = setInterval(async () => {
    if (isCleanedUp) return;

    try {
      const [{ cpuUsage, newStats }, mem, conns, webrtc, sbVersion, awgPeers] = await Promise.all([
        getCpuUsage(streamCpuStats),
        getMemoryUsage(),
        getConnectionCount(),
        getWebRtcStatus(),
        getSingBoxVersionCached(),
        getAwgActivePeersCount()
      ]);
      streamCpuStats = newStats;

      if (isCleanedUp) return;

      call.write({
        cpuUsage,
        memUsage: mem,
        activeConnections: conns,
        systemLogs: logBuffer,
        timestamp: Date.now(),
        webrtcStatus: webrtc,
        singboxVersion: sbVersion,
        awgActivePeers: awgPeers
      });

      logBuffer = ''; 
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      logger.debug({ err: msg }, 'Error inside telemetry streaming interval tick');
    }
  }, 2000);
}

/**
 * RPC Обработчик UploadSingboxBinary (клиентский стрим RPC)
 */
async function uploadSingboxBinaryHandler(
  call: ServerReadableStream<BinaryChunkPayload, UpgradeResponse>,
  callback: sendUnaryData<UpgradeResponse>
): Promise<void> {
  const tempPath = '/tmp/sing-box.download';
  let secretVerified = false;
  let targetVersion = 'unknown';
  let bytesWritten = 0;
  let fileStream: ReturnType<typeof createWriteStream> | null = null;

  const metadataSecret = extractSecretFromMetadata(call);
  if (metadataSecret === config.EGRESS_CONTROL_SECRET) {
    secretVerified = true;
  }

  call.on('data', (data: BinaryChunkPayload) => {
    if (!secretVerified) {
      if (data.orchestratorSecret === config.EGRESS_CONTROL_SECRET || data.orchestrator_secret === config.EGRESS_CONTROL_SECRET) {
        secretVerified = true;
      }
    }
    if (data.version) {
      targetVersion = data.version;
    }
    if (data.chunk && data.chunk.length > 0) {
      if (!fileStream) {
        fileStream = createWriteStream(tempPath);
      }
      const buf = Buffer.from(data.chunk);
      fileStream.write(buf);
      bytesWritten += buf.length;
    }
  });

  call.on('end', async () => {
    if (fileStream) {
      await new Promise<void>((resolve) => fileStream!.end(resolve));
    }

    if (!secretVerified) {
      await fs.unlink(tempPath).catch(() => {});
      logger.warn('Unauthorized UploadSingboxBinary attempt rejected');
      return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
    }

    if (bytesWritten === 0) {
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    try {
      const targetPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      if (process.env.NODE_ENV !== 'test') {
        // Проверяем валидность скачанного файла
        await execAsync(`${tempPath} version`);

        if (process.platform === 'linux') {
          try {
            await execAsync(`setcap 'cap_net_admin,cap_net_bind_service=+ep' ${tempPath}`);
          } catch (err: any) {
            logger.warn({ err: err.message }, 'Failed to setcap on new sing-box binary');
          }
        }
      }

      await fs.mkdir(path.dirname(targetPath), { recursive: true });
      await fs.rename(tempPath, targetPath).catch(async () => {
        // Fallback for cross-device rename
        await fs.copyFile(tempPath, targetPath);
        await fs.unlink(tempPath).catch(() => {});
      });

      logger.info({ path: targetPath, version: targetVersion }, 'Atomically updated sing-box binary');
      cachedSingboxVersion = '';

      if (process.env.NODE_ENV !== 'test') {
        try {
          const reloadCmd = config.RELOAD_COMMAND || 'systemctl restart sing-box';
          const { stdout, stderr } = await execAsync(reloadCmd);
          if (stdout) logger.info({ stdout }, 'Restart/Reload after binary upgrade');
          if (stderr) logger.warn({ stderr }, 'Restart/Reload stderr');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Reload command failed after binary upgrade');
        }
      }

      return callback(null, {
        success: true,
        message: `sing-box binary version ${targetVersion} successfully updated and restarted`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded sing-box binary');
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadSingboxBinary stream');
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  });
}

/**
 * RPC Обработчик UploadOlcrtcBinary (универсальный клиентский стрим RPC для olcrtc / olcrtc-manager)
 */
async function uploadOlcrtcBinaryHandler(
  call: ServerReadableStream<BinaryChunkPayload, UploadBinaryResponse>,
  callback: sendUnaryData<UploadBinaryResponse>
): Promise<void> {
  let secretVerified = false;
  let targetVersion = 'unknown';
  let targetBinary = 'olcrtc-manager';
  let bytesWritten = 0;

  const ALLOWED_BINARIES = new Set(['olcrtc', 'olcrtc-manager']);
  let fileStream: ReturnType<typeof createWriteStream> | null = null;
  let tempPath = '';

  const metadataSecret = extractSecretFromMetadata(call);
  if (metadataSecret === config.EGRESS_CONTROL_SECRET) {
    secretVerified = true;
  }

  call.on('data', (data: BinaryChunkPayload) => {
    if (!secretVerified) {
      if (data.orchestratorSecret === config.EGRESS_CONTROL_SECRET || data.orchestrator_secret === config.EGRESS_CONTROL_SECRET) {
        secretVerified = true;
      }
    }
    if (data.version) {
      targetVersion = data.version;
    }
    if (data.targetBinary || data.target_binary) {
      targetBinary = (data.targetBinary || data.target_binary)!;
    }
    if (data.chunk && data.chunk.length > 0) {
      if (!fileStream) {
        const safeTargetBinary = ALLOWED_BINARIES.has(targetBinary) ? targetBinary : 'olcrtc-manager';
        tempPath = `/tmp/${safeTargetBinary}.download`;
        fileStream = createWriteStream(tempPath);
      }
      const buf = Buffer.from(data.chunk);
      fileStream.write(buf);
      bytesWritten += buf.length;
    }
  });

  call.on('end', async () => {
    if (fileStream) {
      await new Promise<void>((resolve) => fileStream!.end(resolve));
    }

    if (!secretVerified) {
      if (tempPath) await fs.unlink(tempPath).catch(() => {});
      logger.warn('Unauthorized UploadOlcrtcBinary attempt rejected');
      return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
    }

    if (bytesWritten === 0) {
      if (tempPath) await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    try {
      const safeTargetBinary = ALLOWED_BINARIES.has(targetBinary) ? targetBinary : 'olcrtc-manager';
      let targetPath = (safeTargetBinary === 'olcrtc') 
        ? (config.OLCRTC_BINARY_PATH || '/usr/local/bin/olcrtc')
        : (config.OLCRTC_MANAGER_BINARY_PATH || '/usr/local/bin/olcrtc-manager');

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.chmod(tempPath, 0o755);

      await fs.mkdir(path.dirname(targetPath), { recursive: true });
      await fs.rename(tempPath, targetPath).catch(async () => {
        await fs.copyFile(tempPath, targetPath);
        await fs.unlink(tempPath).catch(() => {});
      });

      logger.info({ path: targetPath, binary: targetBinary, version: targetVersion }, 'Atomically updated olcrtc component binary');

      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync('systemctl restart olcrtc || true');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to restart olcrtc service after binary upload');
        }
      }

      return callback(null, {
        success: true,
        message: `${targetBinary} binary successfully updated to version ${targetVersion}`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded olcrtc binary');
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadOlcrtcBinary stream');
  });
}

/**
 * RPC Обработчик UpgradeSingbox
 */
async function upgradeSingboxHandler(
  call: ServerUnaryCall<UpgradePayload, UploadBinaryResponse>,
  callback: sendUnaryData<UploadBinaryResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized UpgradeSingbox request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { version, downloadUrl, download_url } = call.request;
  const url = downloadUrl || download_url;
  const targetVersion = version || 'latest';

  if (!url) {
    return callback(null, { success: false, message: 'Missing download_url in request payload.' });
  }

  try {
    logger.info({ version: targetVersion, url }, 'Initiating sing-box binary upgrade via download URL...');
    const targetPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
    const tempPath = '/tmp/sing-box.download';

    await execFileAsync('curl', ['-fsSL', url, '-o', tempPath]);
    await fs.chmod(tempPath, 0o755);

    if (process.env.NODE_ENV !== 'test') {
      await execAsync(`${tempPath} version`);
      if (process.platform === 'linux') {
        try {
          await execAsync(`setcap 'cap_net_admin,cap_net_bind_service=+ep' ${tempPath}`);
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to setcap on downloaded sing-box binary');
        }
      }
    }

    await fs.mkdir(path.dirname(targetPath), { recursive: true });
    await fs.rename(tempPath, targetPath).catch(async () => {
      await fs.copyFile(tempPath, targetPath);
      await fs.unlink(tempPath).catch(() => {});
    });

    cachedSingboxVersion = '';
    logger.info({ path: targetPath, version: targetVersion }, 'Successfully upgraded sing-box binary via URL');

    if (process.env.NODE_ENV !== 'test') {
      try {
        const reloadCmd = config.RELOAD_COMMAND || 'systemctl restart sing-box';
        await execAsync(reloadCmd);
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Reload command failed after sing-box upgrade');
      }
    }

    return callback(null, {
      success: true,
      message: `sing-box binary version ${targetVersion} successfully upgraded from ${url}`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg }, 'Failed to upgrade sing-box binary via URL');
    return callback(null, { success: false, message: `Upgrade failed: ${msg}` });
  }
}

/**
 * RPC Обработчик ConfigureCaddy
 */
async function configureCaddyHandler(
  call: ServerUnaryCall<CaddyConfigPayload, CaddyConfigResponse>,
  callback: sendUnaryData<CaddyConfigResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized ConfigureCaddy request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const {
    domains,
    domain,
    decoyPort,
    camouflageHtml,
    camouflage_html,
    htmlContent,
    camouflagePath,
    camouflage_path,
    xhttpRegexp,
    xhttp_regexp,
    xhttpRewrite,
    xhttp_rewrite,
    xhttpSocket,
    xhttp_socket,
    grpcRegexp,
    grpc_regexp,
    grpcRewrite,
    grpc_rewrite,
    grpcSocket,
    grpc_socket
  } = call.request;

  try {
    let rawDomains: string[] = [];
    if (Array.isArray(domains) && domains.length > 0) {
      rawDomains = domains;
    } else if (domain) {
      rawDomains = [domain];
    }

    const uniqueDomains = Array.from(
      new Set(
        rawDomains
          .map((d) => (typeof d === 'string' ? d.trim() : ''))
          .filter((d) => d.length > 0)
      )
    );

    if (uniqueDomains.length === 0) {
      return callback(null, { success: false, message: 'Список доменов пуст' });
    }

    const finalCamouflagePath = camouflagePath || camouflage_path || '/var/www/camouflage';
    const finalCamouflageHtml = camouflageHtml || camouflage_html || htmlContent;

    await fs.mkdir(finalCamouflagePath, { recursive: true });
    if (finalCamouflageHtml && typeof finalCamouflageHtml === 'string' && finalCamouflageHtml.trim().length > 0) {
      await fs.writeFile(path.join(finalCamouflagePath, 'index.html'), finalCamouflageHtml, 'utf-8');
    }

    const rawXhttpRegexp = (xhttpRegexp || xhttp_regexp || '').trim();
    const finalXhttpRegexp = rawXhttpRegexp || '^/xhttp-path.*$';
    const finalXhttpRewrite = xhttpRewrite || xhttp_rewrite;
    const finalXhttpSocket = xhttpSocket || xhttp_socket || 'unix//dev/shm/vless-xhttp.sock';

    const rawGrpcRegexp = (grpcRegexp || grpc_regexp || '').trim();
    const finalGrpcRegexp = rawGrpcRegexp || '^/grpc-path.*$';
    const finalGrpcRewrite = grpcRewrite || grpc_rewrite;
    const finalGrpcSocket = grpcSocket || grpc_socket || 'unix+h2c//dev/shm/vless-grpc.sock';

    let caddyfileContent = '# Auto-generated by Route Agent\n\n';

    for (const d of uniqueDomains) {
      const hostHeader = decoyPort ? `${d}:${decoyPort}` : d;
      caddyfileContent += `${hostHeader} {\n`;
      caddyfileContent += `    log {\n        output stdout\n        format console\n        level DEBUG\n    }\n\n`;

      if (finalXhttpRegexp && finalXhttpSocket) {
        caddyfileContent += `    @vless-xhttp path_regexp xhttp ${finalXhttpRegexp}\n    
    handle @vless-xhttp {\n`;
        if (finalXhttpRewrite) {
          caddyfileContent += `        rewrite * ${finalXhttpRewrite}\n`;
        }
        caddyfileContent += `        
        reverse_proxy ${finalXhttpSocket} {\n`;
        caddyfileContent += `            flush_interval -1\n`;
        caddyfileContent += `            transport http {\n`;
        caddyfileContent += `                versions h2c 2\n`;
        caddyfileContent += `            }\n`;
        caddyfileContent += `        }\n`;
        caddyfileContent += `    }\n\n`;
      }

      if (finalGrpcRegexp && finalGrpcSocket) {
        caddyfileContent += `    @vless-grpc {\n`;
        caddyfileContent += `        protocol grpc\n`;
        caddyfileContent += `        path_regexp grpc ${finalGrpcRegexp}\n`;
        caddyfileContent += `    }\n    
    handle @vless-grpc {\n`;
        if (finalGrpcRewrite) {
          caddyfileContent += `        rewrite * ${finalGrpcRewrite}\n`;
        }
        caddyfileContent += `        
        reverse_proxy ${finalGrpcSocket} {\n`;
        caddyfileContent += `            flush_interval -1\n`;
        caddyfileContent += `        }\n`;
        caddyfileContent += `    }\n\n`;
      }

      caddyfileContent += `    root * ${finalCamouflagePath}\n`;
      caddyfileContent += `    file_server\n`;
      caddyfileContent += `}\n\n`;
    }

    const caddyfilePath = config.CADDYFILE_PATH || '/etc/caddy/Caddyfile';
    const caddyDir = path.dirname(caddyfilePath);
    await fs.mkdir(caddyDir, { recursive: true });
    await fs.writeFile(caddyfilePath, caddyfileContent, 'utf-8');

    await fixCaddyPermissions();

    if (process.env.NODE_ENV !== 'test') {
      try {
        const reloadCmd = config.CADDY_RELOAD_COMMAND || 'systemctl reload caddy || systemctl restart caddy';
        const { stdout, stderr } = await execAsync(reloadCmd);
        if (stdout) logger.info({ stdout }, 'Caddy reload stdout');
        if (stderr) logger.warn({ stderr }, 'Caddy reload stderr');
      } catch (err: unknown) {
        const msg = err instanceof Error ? err.message : String(err);
        logger.warn({ err: msg }, 'Failed to reload Caddy service');
      }
    }

    return callback(null, {
      success: true,
      message: `Caddyfile и заглушка успешно обновлены для доменов: ${uniqueDomains.join(', ')}`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg }, 'Failed to configure Caddy');
    return callback(null, { success: false, message: msg });
  }
}

/**
 * RPC Обработчик ConfigureOlcrtc (настройка и управление службой olcrtc-manager)
 */
async function configureOlcrtcHandler(
  call: ServerUnaryCall<OlcrtcConfigPayload, OlcrtcConfigResponse>,
  callback: sendUnaryData<OlcrtcConfigResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized ConfigureOlcrtc request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { enabled, user, password, port } = call.request;

  try {
    if (!enabled) {
      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync('systemctl stop olcrtc || true');
          await execAsync('systemctl disable olcrtc || true');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Error stopping/disabling olcrtc service');
        }
      }
      return callback(null, {
        success: true,
        message: 'olcrtc service successfully disabled and stopped.'
      });
    }

    const servicePort = port || 8888;
    const managerBin = config.OLCRTC_MANAGER_BINARY_PATH || '/usr/local/bin/olcrtc-manager';

    if (process.env.NODE_ENV !== 'test') {
      const managerExists = await fs.stat(managerBin).then(() => true).catch(() => false);
      if (!managerExists) {
        logger.warn({ path: managerBin }, 'olcrtc-manager binary is missing when configuring service');
      }
    }

    const serviceContent = `[Unit]
Description=OpenLibreCommunity WebRTC Manager Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=${managerBin} --port ${servicePort}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`;

    const servicePath = '/etc/systemd/system/olcrtc.service';
    await fs.mkdir(path.dirname(servicePath), { recursive: true }).catch(() => {});
    await fs.writeFile(servicePath, serviceContent, 'utf-8');

    if (process.env.NODE_ENV !== 'test') {
      await execAsync('systemctl daemon-reload');
      await execAsync('systemctl enable --now olcrtc');

      // Открываем порт в UFW
      if (await isUfwInstalled()) {
        try {
          await execAsync(`sudo ufw allow ${servicePort}`);
          await execAsync('sudo ufw reload');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to configure UFW port for olcrtc service');
        }
      }

      if (user && password) {
        // Поллинг готовности API
        const authMeUrl = `http://127.0.0.1:${servicePort}/api/auth/me`;
        const setupUrl = `http://127.0.0.1:${servicePort}/api/auth/setup`;

        for (let i = 0; i < 10; i++) {
          try {
            await getJson(authMeUrl, 1000);
            break;
          } catch {
            await new Promise((res) => setTimeout(res, 500));
          }
        }

        try {
          await new Promise<void>((resolve, reject) => {
            const reqData = JSON.stringify({ user, password });
            const req = http.request(setupUrl, {
              method: 'POST',
              headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(reqData)
              }
            }, (res) => {
              res.resume();
              resolve();
            });
            req.on('error', reject);
            req.write(reqData);
            req.end();
          });
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to POST auth setup to olcrtc-manager');
        }
      }
    }

    return callback(null, {
      success: true,
      message: `olcrtc service configured and enabled on port ${servicePort}.`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to configure Olcrtc service');
    return callback(null, { success: false, message: `Olcrtc configuration error: ${msg}` });
  }
}

/**
 * RPC Обработчик ConfigureAwg (дистанционная настройка и управление L3-интерфейсом AmneziaWG 3.0)
 */
async function configureAwgHandler(
  call: ServerUnaryCall<AwgConfigPayload, AwgConfigResponse>,
  callback: sendUnaryData<AwgConfigResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized ConfigureAwg request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const {
    enabled,
    port,
    serverPrivateKey,
    serverPublicKey,
    addressV4,
    addressV6,
    jc,
    jmin,
    jmax,
    s1,
    s2,
    s3,
    s4,
    h1,
    h2,
    h3,
    h4,
    headerProtectionKey,
    peers,
    ipv6Mode
  } = call.request;

  try {
    const awgConfigPath = config.AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/awg0.conf';

    if (!enabled) {
      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync('awg-quick down awg0 || systemctl stop awg-quick@awg0 || true');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Error stopping AmneziaWG interface');
        }

        if (port && (await isUfwInstalled())) {
          try {
            await execAsync(`sudo ufw delete allow ${port}/udp || true`);
            await execAsync('sudo ufw reload || true');
          } catch (err: any) {
            logger.warn({ err: err.message }, 'Error closing UFW port for AmneziaWG');
          }
        }
      }

      return callback(null, {
        success: true,
        message: 'AmneziaWG (AWG3) service successfully disabled and stopped.'
      });
    }

    // 1. Системные настройки (Sysctl)
    if (process.env.NODE_ENV !== 'test') {
      try {
        await execAsync('sysctl -w net.ipv4.ip_forward=1');
        if (ipv6Mode === 'dual-stack' || ipv6Mode === 'trap-ipv6') {
          await execAsync('sysctl -w net.ipv6.conf.all.forwarding=1');
        }
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to set sysctl packet forwarding');
      }
    }

    // 2. Генерация конфигурации /etc/amnezia/amneziawg/awg0.conf
    const addresses: string[] = [];
    if (addressV4) addresses.push(addressV4);
    if (addressV6 && ipv6Mode !== 'ipv4-only') addresses.push(addressV6);

    let configContent = '[Interface]\n';
    if (serverPrivateKey) configContent += `PrivateKey = ${serverPrivateKey}\n`;
    if (port) configContent += `ListenPort = ${port}\n`;
    if (addresses.length > 0) configContent += `Address = ${addresses.join(', ')}\n`;

    if (jc !== undefined && jc !== null) configContent += `Jc = ${jc}\n`;
    if (jmin !== undefined && jmin !== null) configContent += `Jmin = ${jmin}\n`;
    if (jmax !== undefined && jmax !== null) configContent += `Jmax = ${jmax}\n`;
    if (s1 !== undefined && s1 !== null) configContent += `S1 = ${s1}\n`;
    if (s2 !== undefined && s2 !== null) configContent += `S2 = ${s2}\n`;
    if (s3 !== undefined && s3 !== null) configContent += `S3 = ${s3}\n`;
    if (s4 !== undefined && s4 !== null) configContent += `S4 = ${s4}\n`;
    if (h1 !== undefined && h1 !== null) configContent += `H1 = ${h1}\n`;
    if (h2 !== undefined && h2 !== null) configContent += `H2 = ${h2}\n`;
    if (h3 !== undefined && h3 !== null) configContent += `H3 = ${h3}\n`;
    if (h4 !== undefined && h4 !== null) configContent += `H4 = ${h4}\n`;
    if (headerProtectionKey) configContent += `HeaderProtectionKey = ${headerProtectionKey}\n`;

    if (Array.isArray(peers)) {
      for (const peer of peers) {
        if (!peer || !peer.publicKey) continue;
        configContent += '\n[Peer]\n';
        configContent += `PublicKey = ${peer.publicKey}\n`;
        if (peer.presharedKey) configContent += `PresharedKey = ${peer.presharedKey}\n`;
        if (peer.allowedIps) configContent += `AllowedIPs = ${peer.allowedIps}\n`;
      }
    }

    await fs.mkdir(path.dirname(awgConfigPath), { recursive: true });
    await fs.writeFile(awgConfigPath, configContent, 'utf-8');

    if (process.env.NODE_ENV !== 'test') {
      // 3. Применение без разрыва сессий (Hot Reload)
      let interfaceExists = false;
      try {
        await execAsync('ip link show awg0');
        interfaceExists = true;
      } catch {
        interfaceExists = false;
      }

      if (interfaceExists) {
        try {
          await execAsync(`bash -c "awg syncconf awg0 <(awg-quick strip ${awgConfigPath})"` );
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed hot reload via awg syncconf, attempting awg-quick restart');
          await execAsync(`awg-quick down awg0 || true && awg-quick up ${awgConfigPath}`);
        }
      } else {
        await execAsync(`awg-quick up ${awgConfigPath}`);
      }

      // 4. Настройка NAT и Маршрутизации (iptables)
      try {
        await execAsync('iptables -C FORWARD -i awg0 -j ACCEPT || iptables -A FORWARD -i awg0 -j ACCEPT');
        await execAsync("iptables -t nat -C POSTROUTING -o $(ip route show default | awk '/default/ {print $5}') -j MASQUERADE || iptables -t nat -A POSTROUTING -o $(ip route show default | awk '/default/ {print $5}') -j MASQUERADE");
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to configure iptables NAT rules for awg0');
      }

      // 5. Защита от утечек IPv6 (trap-ipv6)
      if (ipv6Mode === 'trap-ipv6') {
        try {
          await execAsync('ip6tables -C FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited || ip6tables -A FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to configure ip6tables trap-ipv6 rules for awg0');
        }
      }

      // 6. Открытие фаервола
      if (port && (await isUfwInstalled())) {
        try {
          await execAsync(`sudo ufw allow ${port}/udp`);
          await execAsync('sudo ufw reload');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to configure UFW port for AmneziaWG service');
        }
      }
    }

    return callback(null, {
      success: true,
      message: `AmneziaWG (AWG3) service configured and enabled on port ${port || 51820}.`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to configure AmneziaWG service');
    return callback(null, { success: false, message: `AmneziaWG configuration error: ${msg}` });
  }
}

/**
 * RPC Обработчик ManageFirewall
 */
async function manageFirewallHandler(
  call: ServerUnaryCall<FirewallPayload, FirewallResponse>,
  callback: sendUnaryData<FirewallResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized ManageFirewall request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const openUdp = Array.isArray(call.request.openUdpPorts) ? call.request.openUdpPorts : [];
  const openTcp = Array.isArray(call.request.openTcpPorts) ? call.request.openTcpPorts : [];

  try {
    const ufwAvailable = await isUfwInstalled();
    if (!ufwAvailable) {
      if (process.env.NODE_ENV === 'test') {
        return callback(null, {
          success: true,
          message: `UFW not installed. (Test mode dry-run: UDP [${openUdp.join(', ')}], TCP [${openTcp.join(', ')}])`
        });
      }
      logger.warn('UFW is not installed on this node');
      return callback(null, { success: false, message: 'UFW firewall utility is not installed on system.' });
    }

    for (const port of openUdp) {
      try {
        await execAsync(`sudo ufw allow ${port}/udp`);
      } catch (err: any) {
        logger.error({ port, err: err.message }, 'Failed to open UDP port in firewall');
      }
    }

    for (const port of openTcp) {
      try {
        await execAsync(`sudo ufw allow ${port}/tcp`);
      } catch (err: any) {
        logger.error({ port, err: err.message }, 'Failed to open TCP port in firewall');
      }
    }

    try {
      await execAsync('sudo ufw reload');
    } catch (err: any) {
      logger.warn({ err: err.message }, 'Failed to reload UFW');
    }

    return callback(null, {
      success: true,
      message: `Successfully updated firewall rules: opened ${openUdp.length} UDP and ${openTcp.length} TCP ports.`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to execute ManageFirewall');
    return callback(null, { success: false, message: `ManageFirewall error: ${msg}` });
  }
}

/**
 * RPC Обработчик SelfUpdate
 */
async function selfUpdateHandler(
  call: ServerUnaryCall<SelfUpdatePayload, SelfUpdateResponse>,
  callback: sendUnaryData<SelfUpdateResponse>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    logger.warn('Unauthorized SelfUpdate request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  // 1. Отправляем успешный ответ клиенту до перезапуска службы
  callback(null, {
    success: true,
    message: 'Self-update sequence initiated. Agent will pull code and restart in 1 second.'
  });

  // 2. Асинхронно запускаем процесс обновления с задержкой в 1 секунду
  setTimeout(async () => {
    try {
      logger.info('Starting agent self-update sequence...');
      const updateCmd = 'cd /opt/route-agent && git fetch --all && git reset --hard origin/main && git clean -fd && npm ci && npm run build && systemctl restart route-agent';
      await execAsync(updateCmd);
    } catch (err: any) {
      logger.error({ err: err.stderr || err.message }, 'Failed to execute self-update sequence');
    }
  }, 1000);
}

export let serverInstance: Server | null = null;

/**
 * Инициализация gRPC ServerCredentials (mTLS с проверкой клиентских сертификатов или insecure fallback)
 */
async function getGrpcServerCredentials(): Promise<{ credentials: ServerCredentials; isMtls: boolean }> {
  try {
    const { CA_CERT_PATH, AGENT_CERT_PATH, AGENT_KEY_PATH } = config;
    const [caExists, certExists, keyExists] = await Promise.all([
      fs.stat(CA_CERT_PATH).then(() => true).catch(() => false),
      fs.stat(AGENT_CERT_PATH).then(() => true).catch(() => false),
      fs.stat(AGENT_KEY_PATH).then(() => true).catch(() => false),
    ]);

    if (caExists && certExists && keyExists) {
      const [caCert, agentCert, agentKey] = await Promise.all([
        fs.readFile(CA_CERT_PATH),
        fs.readFile(AGENT_CERT_PATH),
        fs.readFile(AGENT_KEY_PATH),
      ]);

      logger.info({ caPath: CA_CERT_PATH, certPath: AGENT_CERT_PATH }, '🔒 Enabling mTLS with strict client certificate verification');

      const credentials = ServerCredentials.createSsl(
        caCert,
        [{ cert_chain: agentCert, private_key: agentKey }],
        true
      );
      return { credentials, isMtls: true };
    }
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.warn({ err: msg }, 'Failed to load mTLS certificates; falling back to insecure gRPC channel');
  }

  logger.warn('⚠️ mTLS certificates not found or incomplete. Starting gRPC server in insecure mode');
  return { credentials: ServerCredentials.createInsecure(), isMtls: false };
}

export async function startServer(): Promise<Server> {
  const { credentials, isMtls } = await getGrpcServerCredentials();
  return new Promise((resolve, reject) => {
    const server = new Server();
    const serviceImplementation: UntypedServiceImplementation = {
      applyConfig: applyConfigHandler,
      streamTelemetry: streamTelemetryHandler,
      upgradeSingbox: upgradeSingboxHandler,
      uploadSingboxBinary: uploadSingboxBinaryHandler,
      uploadOlcrtcBinary: uploadOlcrtcBinaryHandler,
      configureCaddy: configureCaddyHandler,
      configureOlcrtc: configureOlcrtcHandler,
      configureAwg: configureAwgHandler,
      manageFirewall: manageFirewallHandler,
      selfUpdate: selfUpdateHandler
    };
    
    server.addService(agentPackage.EgressAgentService.service, serviceImplementation);

    const bindTarget = `${config.HOST}:${config.PORT}`;
    server.bindAsync(bindTarget, credentials, (err, port) => {
      if (err) {
        logger.error({ err }, 'Failed to bind gRPC server');
        reject(err);
        return;
      }
      const scheme = isMtls ? 'h2' : 'h2c';
      logger.info(`🚀 gRPC Route Agent actively listening at ${scheme}://${config.HOST}:${port}`);
      serverInstance = server;
      resolve(server);
    });
  });
}

if (process.env.NODE_ENV !== 'test') {
  startServer().catch(() => {
    process.exit(1);
  });
}
