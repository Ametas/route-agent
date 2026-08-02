import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import { execAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';
import { computeProtoContractHash } from './protoContract.service.js';

const logger = pino({ level: 'info' });

let cachedAgentVersion: string | null = null;

export async function getAgentVersion(): Promise<string> {
  if (cachedAgentVersion) return cachedAgentVersion;

  let version = '';
  try {
    const { stdout } = await execAsync('git rev-parse --short HEAD');
    if (stdout && stdout.trim()) {
      version = stdout.trim();
    }
  } catch {}

  if (!version) {
    try {
      const pkgPath = path.resolve(process.cwd(), 'package.json');
      const pkgContent = await fs.readFile(pkgPath, 'utf-8');
      const pkg = JSON.parse(pkgContent);
      if (pkg.version && typeof pkg.version === 'string') {
        version = pkg.version;
      }
    } catch {}
  }

  if (!version) {
    version = '1.0.0';
  }

  cachedAgentVersion = version;
  return version;
}

/**
 * RPC Обработчик GetAgentInfo
 * 
 * ВАЖНО: Данный эндпоинт сознательно исполняется БЕЗ проверки x-orchestrator-secret.
 * Это read-only диагностический метод, используемый оркестратором для pre-flight
 * валидации совместимости контракта Protobuf и версии агента до проведения
 * мутирующих RPC вызовов (включая ситуации первичного подключения и диагностики).
 */
export async function getAgentInfoHandler(
  _call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  try {
    const protoContractHash = computeProtoContractHash();
    const agentVersion = await getAgentVersion();
    return callback(null, {
      protoContractHash,
      proto_contract_hash: protoContractHash,
      agentVersion,
      agent_version: agentVersion,
      protoContractSource: 'canonical-json',
      proto_contract_source: 'canonical-json',
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to process GetAgentInfo RPC');
    return callback(null, {
      protoContractHash: '',
      proto_contract_hash: '',
      agentVersion: '',
      agent_version: '',
      protoContractSource: 'canonical-json',
      proto_contract_source: 'canonical-json',
    });
  }
}

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
