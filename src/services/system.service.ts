import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { execAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';

const logger = pino({ level: 'info' });

/**
 * RPC Обработчик SelfUpdate
 */
export async function selfUpdateHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
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
      const updateCmd = 'cd /opt/route-agent && git fetch --all && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent';
      await execAsync(updateCmd);
    } catch (err: any) {
      logger.error({ err: err.stderr || err.message }, 'Failed to execute self-update sequence');
    }
  }, 1000);
}
