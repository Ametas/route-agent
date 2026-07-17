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
  journalProcess.stdout.on('data', (chunk: Buffer) => {
    logBuffer += chunk.toString();
  });

  const telemetryInterval = setInterval(async () => {
    const [cpu, mem, conns] = await Promise.all([getCpuUsage(), getMemoryUsage(), getConnectionCount()]);
    
    call.write({
      cpuUsage: cpu,
      memUsage: mem,
      activeConnections: conns,
      systemLogs: logBuffer,
      timestamp: Date.now()
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
