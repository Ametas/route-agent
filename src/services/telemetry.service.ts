import { ServerWritableStream } from '@grpc/grpc-js';
import { spawn } from 'child_process';
import pino from 'pino';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';
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
  const secretHeader = extractSecretFromMetadata(call) || call.request?.orchestratorSecret;

  if (!verifySecret(secretHeader)) {
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

  const sendTelemetry = async () => {
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
  };

  // Immediate initial push, then stream every 2s
  sendTelemetry();
  telemetryInterval = setInterval(sendTelemetry, 2000);
}
