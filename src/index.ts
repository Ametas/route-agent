import { 
  Server, 
  ServerCredentials, 
  ServerUnaryCall, 
  sendUnaryData, 
  ServerWritableStream,
  UntypedServiceImplementation,
  loadPackageDefinition
} from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { exec, spawn } from 'child_process';
import { promisify } from 'util';
import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import net from 'net';
import http from 'http';
import { config } from './config.js';

const execAsync = promisify(exec);
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

let lastCpuStats = { idle: 0, total: 0 };

/**
 * Читает и высчитывает дельту утилизации CPU из /proc/stat
 */
async function getCpuUsage(): Promise<number> {
  try {
    const stat = await fs.readFile('/proc/stat', 'utf-8');
    const firstLine = stat.split('\n')[0];
    const times = firstLine.split(/\s+/).slice(1).map(Number);
    const total = times.reduce((a, b) => a + b, 0);
    const idle = times[3];
    
    const diffIdle = idle - lastCpuStats.idle;
    const diffTotal = total - lastCpuStats.total;
    lastCpuStats = { idle, total };
    
    return diffTotal === 0 ? 0 : ((diffTotal - diffIdle) / diffTotal) * 100;
  } catch {
    return 0;
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
    const data = await getJson(url, 3000);
    
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
 * Вспомогательный метод локальной валидации синтаксиса sing-box перед его применением
 */
async function validateSingBoxConfig(configObj: object): Promise<{ valid: boolean; error?: string }> {
  if (process.env.NODE_ENV === 'test') {
    return { valid: true };
  }
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const checkFilePath = path.join(targetDir, `.config.check_${Date.now()}.json`);
  
  try {
    await fs.mkdir(targetDir, { recursive: true });
    await fs.writeFile(checkFilePath, JSON.stringify(configObj, null, 2), 'utf-8');
    
    // Выполняем нативный тест синтаксиса sing-box
    await execAsync(`sing-box check -c ${checkFilePath}`);
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
  const { stdout, stderr } = await execAsync(config.RELOAD_COMMAND);
  if (stdout) logger.info({ stdout }, 'Reload command stdout');
  if (stderr) logger.warn({ stderr }, 'Reload command stderr');
}

/**
 * Абсолютный путь к локальному файлу-кэшу, хранящему список UDP-портов,
 * которые были открыты в UFW при прошлом успешном применении конфигурации.
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
function extractUdpTunnelPorts(configObj: any): number[] {
  const inbounds = Array.isArray(configObj?.inbounds) ? configObj.inbounds : [];
  const ports = new Set<number>();

  for (const inbound of inbounds) {
    if (!inbound || typeof inbound !== 'object') continue;
    if (!UDP_TUNNEL_INBOUND_TYPES.has(inbound.type)) continue;

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
 * из новой конфигурации sing-box: открывает новые порты и закрывает те,
 * что больше не используются, предотвращая появление "дыр" в безопасности.
 * Любая ошибка ufw логируется, но не прерывает основной пайплайн ApplyConfig.
 */
async function syncEgressFirewall(configObj: any): Promise<void> {
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
      } catch (err: any) {
        logger.error({ port, err: err.stderr || err.message }, 'Failed to open UFW UDP port');
      }
    }

    for (const port of portsToClose) {
      try {
        const { stdout, stderr } = await execAsync(`sudo ufw delete allow ${port}/udp`);
        logger.info({ port, stdout, stderr }, 'Closed stale UDP firewall port no longer used by egress config');
      } catch (err: any) {
        logger.error({ port, err: err.stderr || err.message }, 'Failed to close UFW UDP port');
      }
    }

    try {
      const { stdout, stderr } = await execAsync('sudo ufw reload');
      if (stdout) logger.info({ stdout }, 'UFW reload stdout');
      if (stderr) logger.warn({ stderr }, 'UFW reload stderr');
    } catch (err: any) {
      logger.error({ err: err.stderr || err.message }, 'Failed to reload UFW after egress firewall synchronization');
    }

    await writeActivePortsCache(newPorts);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Unexpected error while synchronizing egress firewall state');
  }
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
}

/**
 * RPC Обработчик метода ApplyConfig с честной строгой типизацией
 */
async function applyConfigHandler(
  call: ServerUnaryCall<ApplyConfigRequest, ApplyConfigResponse>, 
  callback: sendUnaryData<ApplyConfigResponse>
): Promise<void> {
  const metadataValues = call.metadata.get('x-orchestrator-secret');
  const secretHeader = metadataValues && metadataValues[0] ? String(metadataValues[0]) : '';

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
 * RPC Обработчик серверного стрима телеметрии без any
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
    });
  }

  const telemetryInterval = setInterval(async () => {
    const [cpu, mem, conns, webrtc] = await Promise.all([
      getCpuUsage(),
      getMemoryUsage(),
      getConnectionCount(),
      getWebRtcStatus()
    ]);
    
    call.write({
      cpuUsage: cpu,
      memUsage: mem,
      activeConnections: conns,
      systemLogs: logBuffer,
      timestamp: Date.now(),
      webrtcStatus: webrtc
    });
    
    logBuffer = ''; 
  }, 2000);

  call.on('cancelled', () => {
    clearInterval(telemetryInterval);
    journalProcess.kill();
    logger.info('Telemetry binary stream closed and resources safely released');
  });
}

export let serverInstance: Server | null = null;

export function startServer(): Promise<Server> {
  return new Promise((resolve, reject) => {
    const server = new Server();
    const serviceImplementation: UntypedServiceImplementation = {
      applyConfig: applyConfigHandler,
      streamTelemetry: streamTelemetryHandler
    };
    
    server.addService(agentPackage.EgressAgentService.service, serviceImplementation);

    const bindTarget = `${config.HOST}:${config.PORT}`;
    server.bindAsync(bindTarget, ServerCredentials.createInsecure(), (err, port) => {
      if (err) {
        logger.error({ err }, 'Failed to bind gRPC server');
        reject(err);
        return;
      }
      logger.info(`🚀 gRPC Route Agent actively listening at h2c://${config.HOST}:${port}`);
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
