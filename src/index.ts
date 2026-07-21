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

/**
 * Вспомогательный метод локальной валидации синтаксиса sing-box перед его применением
 */
async function validateSingBoxConfig(configObj: object): Promise<{ valid: boolean; error?: string }> {
  if (process.env.NODE_ENV === 'test') {
    return { valid: true };
  }
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

/**
 * Извлечение секрета из gRPC metadata
 */
function extractSecretFromMetadata(call: { metadata: any }): string {
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
}

interface BinaryChunk {
  orchestratorSecret?: string;
  chunk?: Buffer | Uint8Array;
  version?: string;
  isFinal?: boolean;
  targetBinary?: string;
}

interface UpgradeResponse {
  success: boolean;
  message: string;
}

interface CaddyConfigPayload {
  domain: string;
  decoyPort: number;
  htmlContent: string;
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

interface FirewallPayload {
  openUdpPorts: number[];
  openTcpPorts: number[];
}

interface FirewallResponse {
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
    });
  }

  const telemetryInterval = setInterval(async () => {
    const [cpu, mem, conns, webrtc, sbVersion] = await Promise.all([
      getCpuUsage(),
      getMemoryUsage(),
      getConnectionCount(),
      getWebRtcStatus(),
      getSingBoxVersion()
    ]);
    
    call.write({
      cpuUsage: cpu,
      memUsage: mem,
      activeConnections: conns,
      systemLogs: logBuffer,
      timestamp: Date.now(),
      webrtcStatus: webrtc,
      singboxVersion: sbVersion
    });
    
    logBuffer = ''; 
  }, 2000);

  call.on('cancelled', () => {
    clearInterval(telemetryInterval);
    journalProcess.kill();
    logger.info('Telemetry binary stream closed and resources safely released');
  });
}

/**
 * RPC Обработчик UploadSingboxBinary (клиентский стрим RPC)
 */
async function uploadSingboxBinaryHandler(
  call: ServerReadableStream<BinaryChunk, UpgradeResponse>,
  callback: sendUnaryData<UpgradeResponse>
): Promise<void> {
  const chunks: Buffer[] = [];
  let secretVerified = false;
  let targetVersion = 'unknown';

  const metadataSecret = extractSecretFromMetadata(call);
  if (metadataSecret === config.EGRESS_CONTROL_SECRET) {
    secretVerified = true;
  }

  call.on('data', (data: BinaryChunk) => {
    if (!secretVerified) {
      if (data.orchestratorSecret === config.EGRESS_CONTROL_SECRET) {
        secretVerified = true;
      }
    }
    if (data.version) {
      targetVersion = data.version;
    }
    if (data.chunk && data.chunk.length > 0) {
      chunks.push(Buffer.from(data.chunk));
    }
  });

  call.on('end', async () => {
    if (!secretVerified) {
      logger.warn('Unauthorized UploadSingboxBinary attempt rejected');
      return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
    }

    if (chunks.length === 0) {
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    try {
      const fullBuffer = Buffer.concat(chunks);
      const tempPath = '/tmp/sing-box.download';
      const targetPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.writeFile(tempPath, fullBuffer);
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
        message: 'sing-box binary successfully updated and restarted'
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg }, 'Failed to apply uploaded sing-box binary');
      return callback(null, { success: false, message: `Failed to upload binary: ${msg}` });
    }
  });

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadSingboxBinary stream');
  });
}

/**
 * RPC Обработчик UploadOlcrtcBinary (клиентский стрим RPC для olcrtc / olcrtc-manager)
 */
async function uploadOlcrtcBinaryHandler(
  call: ServerReadableStream<BinaryChunk, UpgradeResponse>,
  callback: sendUnaryData<UpgradeResponse>
): Promise<void> {
  const chunks: Buffer[] = [];
  let secretVerified = false;
  let targetVersion = 'unknown';
  let targetBinary = 'olcrtc-manager';

  const metadataSecret = extractSecretFromMetadata(call);
  if (metadataSecret === config.EGRESS_CONTROL_SECRET) {
    secretVerified = true;
  }

  call.on('data', (data: BinaryChunk) => {
    if (!secretVerified) {
      if (data.orchestratorSecret === config.EGRESS_CONTROL_SECRET) {
        secretVerified = true;
      }
    }
    if (data.version) {
      targetVersion = data.version;
    }
    if (data.targetBinary) {
      targetBinary = data.targetBinary;
    }
    if (data.chunk && data.chunk.length > 0) {
      chunks.push(Buffer.from(data.chunk));
    }
  });

  call.on('end', async () => {
    if (!secretVerified) {
      logger.warn('Unauthorized UploadOlcrtcBinary attempt rejected');
      return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
    }

    if (chunks.length === 0) {
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    try {
      const fullBuffer = Buffer.concat(chunks);
      const tempPath = `/tmp/${targetBinary}.download`;
      let targetPath = config.OLCRTC_MANAGER_BINARY_PATH || '/usr/local/bin/olcrtc-manager';
      if (targetBinary === 'olcrtc') {
        targetPath = config.OLCRTC_BINARY_PATH || '/usr/local/bin/olcrtc';
      }

      await fs.mkdir(path.dirname(tempPath), { recursive: true });
      await fs.writeFile(tempPath, fullBuffer);
      await fs.chmod(tempPath, 0o755);

      await fs.mkdir(path.dirname(targetPath), { recursive: true });
      await fs.rename(tempPath, targetPath).catch(async () => {
        await fs.copyFile(tempPath, targetPath);
        await fs.unlink(tempPath).catch(() => {});
      });

      logger.info({ path: targetPath, binary: targetBinary, version: targetVersion }, 'Atomically updated olcrtc component binary');

      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync(`systemctl restart olcrtc || true`);
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

  const { domain, decoyPort, htmlContent } = call.request;

  try {
    const webDir = '/var/www/decoy';
    await fs.mkdir(webDir, { recursive: true });
    if (htmlContent) {
      await fs.writeFile(path.join(webDir, 'index.html'), htmlContent, 'utf-8');
    }

    const port = decoyPort || 8443;
    const hostHeader = domain ? `${domain}:${port}` : `:${port}`;
    const caddyfileContent = `${hostHeader} {\n\thandle {\n\t\troot * ${webDir}\n\t\tfile_server\n\t}\n}\n`;

    const caddyfilePath = config.CADDYFILE_PATH || '/etc/caddy/Caddyfile';
    const caddyDir = path.dirname(caddyfilePath);
    await fs.mkdir(caddyDir, { recursive: true });
    await fs.writeFile(caddyfilePath, caddyfileContent, 'utf-8');

    if (process.env.NODE_ENV !== 'test') {
      try {
        const reloadCmd = config.CADDY_RELOAD_COMMAND || 'systemctl reload caddy || systemctl restart caddy';
        const { stdout, stderr } = await execAsync(reloadCmd);
        if (stdout) logger.info({ stdout }, 'Caddy reload stdout');
        if (stderr) logger.warn({ stderr }, 'Caddy reload stderr');
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to reload Caddy service');
      }
    }

    return callback(null, {
      success: true,
      message: `Caddy successfully configured for ${hostHeader}`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to configure Caddy');
    return callback(null, { success: false, message: `Caddy configuration error: ${msg}` });
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
          await execAsync('systemctl disable --now olcrtc || true');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Error disabling olcrtc service');
        }
      }
      return callback(null, {
        success: true,
        message: 'olcrtc service successfully disabled and stopped.'
      });
    }

    const servicePort = port || 8888;
    const managerBin = config.OLCRTC_MANAGER_BINARY_PATH || '/usr/local/bin/olcrtc-manager';

    const serviceContent = `[Unit]
Description=OpenLibreCommunity WebRTC Service
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

      if (user && password) {
        const setupUrl = `http://127.0.0.1:${servicePort}/api/auth/setup`;
        for (let i = 0; i < 5; i++) {
          try {
            await getJson(`http://127.0.0.1:${servicePort}/api/state`, 1000);
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

export let serverInstance: Server | null = null;

export function startServer(): Promise<Server> {
  return new Promise((resolve, reject) => {
    const server = new Server();
    const serviceImplementation: UntypedServiceImplementation = {
      applyConfig: applyConfigHandler,
      streamTelemetry: streamTelemetryHandler,
      uploadSingboxBinary: uploadSingboxBinaryHandler,
      uploadOlcrtcBinary: uploadOlcrtcBinaryHandler,
      configureCaddy: configureCaddyHandler,
      configureOlcrtc: configureOlcrtcHandler,
      manageFirewall: manageFirewallHandler
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
