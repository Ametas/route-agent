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

  /**
   * Версия — от ближайшего тега релиза: `v1.4.0` на самом теге, `v1.4.0-3-gabc1234` на три коммита
   * позже, короткий хеш, если тегов нет вовсе (2026-09-25).
   *
   * Теги ставит semantic-release в CI по типам коммитов (`.releaserc.json`). До этого здесь был
   * голый `git rev-parse --short HEAD`: хеш говорит «какой коммит», но не «новее ли он», и
   * оркестратор не мог решить, умеет ли агент что-то, — ему нечего было сравнить.
   */
  let version = '';
  try {
    const { stdout } = await execAsync('git describe --tags --always');
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
 * ВАЖНО: Поле orchestrator_secret присутствует в контракте сообщения AgentInfoRequest
 * для единообразия gRPC вызовов и на случай ввода строгого режима проверки в будущем.
 * Однако в текущей реализации эндпоинт сознательно исполняется БЕЗ валидации секрета.
 * Это read-only диагностический метод, используемый оркестратором для предварительной
 * проверки (pre-flight check) совместимости Protobuf контракта и версии агента
 * до проведения аутентифицированных мутирующих RPC вызовов (включая ситуации
 * первичного подключения или восстановления рассинхронизированной ноды).
 */
export async function getAgentInfoHandler(
  _call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  try {
    const protoContractHash = computeProtoContractHash();
    const agentVersion = await getAgentVersion();
    return callback(null, {
      proto_contract_hash: protoContractHash,
      protoContractHash: protoContractHash,
      agent_version: agentVersion,
      agentVersion: agentVersion,
      proto_contract_source: 'canonical-json',
      protoContractSource: 'canonical-json',
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to process GetAgentInfo RPC');
    return callback(null, {
      proto_contract_hash: '',
      protoContractHash: '',
      agent_version: '',
      agentVersion: '',
      proto_contract_source: 'canonical-json',
      protoContractSource: 'canonical-json',
    });
  }
}

/**
 * Канал обновления (2026-11-07, решение владельца): `stable` — ветка `main`, `nightly` — ветка `alpha`
 * (Go-агент). Оркестратор не ходит на узлы по SSH, поэтому перевод узла на nightly — тот же
 * SelfUpdate с заголовком `x-agent-channel` в metadata: прото не меняется, хеш договора прежний.
 *
 * Без заголовка — как раньше: подтянуть upstream текущей ветки, ответ байт в байт прежний
 * (в команде добавлен только `--force` у fetch, см. ниже).
 * С заголовком в ответе стоит `channel=<значение>`: по нему оркестратор узнаёт, что агент заголовок
 * понял (старый агент его молча пропускает и отвечает старым текстом).
 */
export const UPDATE_CHANNEL_HEADER = 'x-agent-channel';

/** Канал → ветка. Белый список: в команду попадает только значение отсюда, не ввод запроса. */
export const UPDATE_CHANNEL_BRANCHES: Readonly<Record<string, string>> = Object.freeze({
  stable: 'main',
  nightly: 'alpha',
});

// `--force` у fetch: без него git отказывается сдвигать тег, который на узле уже есть («would
// clobber existing tag»), и обновление встаёт на первом шаге навсегда — каждый следующий fetch
// падает так же. Так ломал узлы подвижный тег пре-релиза Go-агента.
const LEGACY_UPDATE_COMMAND =
  'cd /opt/route-agent && git fetch --all --tags --force && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent';

const LEGACY_UPDATE_MESSAGE = 'Self-update sequence initiated. Agent will pull code and restart in 1 second.';

export type SelfUpdatePlan =
  | { accepted: true; message: string; command: string }
  | { accepted: false; message: string };

/**
 * Значение заголовка канала: `undefined`, если заголовка нет вовсе. Пустое значение — это
 * присланный заголовок, и он отвергается как неизвестный канал.
 */
export function extractUpdateChannelFromMetadata(call: { metadata?: { get(key: string): unknown[] } }): string | undefined {
  const values = call.metadata ? call.metadata.get(UPDATE_CHANNEL_HEADER) : [];
  if (!values || values.length === 0) return undefined;
  const first = values[0];
  return Buffer.isBuffer(first) ? first.toString('utf-8') : String(first);
}

/**
 * Что сделает SelfUpdate: ответ и команда обновления. Чистая функция, чтобы проверять без узла.
 *
 * Смена ветки — `checkout -B <ветка> HEAD`, затем `reset --hard @{u}`, а не сразу `checkout` на
 * удалённую ветку: reset оставляет прежний коммит в ORIG_HEAD, и мост на Go-агента (ветка `alpha`)
 * записывает его для отката.
 */
export function planSelfUpdate(channel: string | undefined): SelfUpdatePlan {
  if (channel === undefined) {
    return { accepted: true, message: LEGACY_UPDATE_MESSAGE, command: LEGACY_UPDATE_COMMAND };
  }

  const branch = Object.prototype.hasOwnProperty.call(UPDATE_CHANNEL_BRANCHES, channel)
    ? UPDATE_CHANNEL_BRANCHES[channel]
    : undefined;
  if (!branch) {
    return { accepted: false, message: `Unknown update channel '${channel.slice(0, 32)}'.` };
  }

  const command = [
    'cd /opt/route-agent',
    `git fetch --force --tags origin +refs/heads/${branch}:refs/remotes/origin/${branch}`,
    `git checkout -q -f -B ${branch} HEAD`,
    `git branch -q --set-upstream-to=origin/${branch} ${branch}`,
    'git reset --hard @{u}',
    'git clean -fd',
    'npm ci',
    'npm run build',
    'systemctl restart route-agent',
  ].join(' && ');

  return {
    accepted: true,
    message: `Self-update sequence initiated (channel=${channel}). Agent will switch to branch ${branch} and restart in 1 second.`,
    command,
  };
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

  const plan = planSelfUpdate(extractUpdateChannelFromMetadata(call));
  if (!plan.accepted) {
    logger.warn({ message: plan.message }, 'SelfUpdate rejected: unknown update channel');
    return callback(null, { success: false, message: plan.message });
  }

  // 1. Отправляем успешный ответ клиенту до перезапуска службы
  callback(null, { success: true, message: plan.message });

  // 2. Асинхронно запускаем процесс обновления с задержкой в 1 секунду
  setTimeout(async () => {
    try {
      logger.info('Starting agent self-update sequence...');
      await execAsync(plan.command);
    } catch (err: any) {
      logger.error({ err: err.stderr || err.message }, 'Failed to execute self-update sequence');
    }
  }, 1000);
}
