import { ServerWritableStream, ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import { spawn } from 'child_process';
import pino from 'pino';
import { authenticateCall } from '../middleware/auth.js';
import {
  getCpuUsage,
  getMemoryUsage,
  getConnectionCount,
  getWebRtcStatus,
  getSingBoxVersionCached,
  getAwgActivePeersCount,
  CpuStats
} from '../utils/telemetry.js';

const logger = pino({ level: 'info' });

/**
 * RPC Обработчик серверного стрима телеметрии
 */
export async function streamTelemetryHandler(
  call: ServerWritableStream<any, any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized gRPC telemetry stream requested');
    const err = new Error('UNAUTHENTICATED: Invalid orchestrator secret token.') as any;
    err.code = 16; // grpc.status.UNAUTHENTICATED
    call.emit('error', err);
    return;
  }

  logger.info('Dynamic telemetry binary stream opened by orchestrator');
  let logBuffer = '';

  const journalProcess = spawn('journalctl', ['-u', 'sing-box', '-n', '10', '-f', '--output', 'cat']);
  journalProcess.on('error', (err: any) => {
    logger.warn({ err: err.message }, 'Failed to spawn journalctl process');
  });

  if (journalProcess.stdout) {
    journalProcess.stdout.on('error', (err: any) => {
      logger.debug({ err: err.message }, 'Journalctl stdout stream error ignored');
    });
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
    if (journalProcess && !journalProcess.killed && journalProcess.pid) {
      try {
        journalProcess.kill('SIGTERM');
      } catch {}
    }
    logger.info('Telemetry binary stream closed and resources safely released');
  };

  // Навешиваем очистку на ВСЕ события закрытия сокета/стрима
  call.on('cancelled', cleanup);
  call.on('close', cleanup);
  call.on('finish', cleanup);
  call.on('error', cleanup);

  const sendTelemetryTick = async (): Promise<void> => {
    if (isCleanedUp || call.destroyed) return;

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

      if (isCleanedUp || call.destroyed) return;

      try {
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
      } catch {
        cleanup();
        return;
      }

      logBuffer = '';
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      logger.debug({ err: msg }, 'Error inside telemetry streaming interval tick');
    }
  };

  // Immediate initial push, then stream every 2s
  await sendTelemetryTick();
  telemetryInterval = setInterval(sendTelemetryTick, 2000);
}

/**
 * RPC Обработчик Unary метода GetTelemetry
 */
export async function getTelemetryHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetTelemetry Unary request blocked');
    return callback(null, {
      cpuUsage: 0,
      memUsage: 0,
      activeConnections: 0,
      systemLogs: 'Error: Unauthorized secret token mismatch',
      timestamp: Date.now(),
      webrtcStatus: 'unauthorized',
      singboxVersion: 'unknown',
      awgActivePeers: 0
    });
  }

  try {
    let streamCpuStats: CpuStats = { idle: 0, total: 0 };
    const [{ cpuUsage }, mem, conns, webrtc, sbVersion, awgPeers] = await Promise.all([
      getCpuUsage(streamCpuStats),
      getMemoryUsage(),
      getConnectionCount(),
      getWebRtcStatus(),
      getSingBoxVersionCached(),
      getAwgActivePeersCount()
    ]);

    return callback(null, {
      cpuUsage,
      memUsage: mem,
      activeConnections: conns,
      systemLogs: 'OK',
      timestamp: Date.now(),
      webrtcStatus: webrtc,
      singboxVersion: sbVersion,
      awgActivePeers: awgPeers
    });
  } catch (err: any) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg }, 'Failed to process GetTelemetry Unary request');
    return callback(null, {
      cpuUsage: 0,
      memUsage: 0,
      activeConnections: 0,
      systemLogs: `Internal Error: ${msg}`,
      timestamp: Date.now(),
      webrtcStatus: 'error',
      singboxVersion: 'error',
      awgActivePeers: 0
    });
  }
}
