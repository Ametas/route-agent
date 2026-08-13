import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { execFileAsync } from '../utils/exec.js';
import { authenticateCall } from '../middleware/auth.js';

const logger = pino({ level: 'info' });

// Диагностика запускается по расписанию (дважды в день) на ограниченном наборе нод —
// корректность/устойчивость данных важнее скорости. 10 циклов дают достаточно
// статистики по Loss%, а таймаут держит худший случай (недостижимая цель, зависший
// mtr) в разумных пределах, не блокируя весь RPC надолго на одной плохой цели.
const MTR_REPORT_CYCLES = 10;
const MTR_TIMEOUT_MS = 45000;

// "???" — стандартный сентинел mtr для хопа, на который вообще не пришёл ответ
// (ни IP, ни hostname не известны). Это НЕ про отказ обратного DNS-резолвинга —
// с флагом --no-dns hostname всегда либо голый IP, либо этот сентинел.
const UNRESOLVED_HOST_SENTINEL = '???';

// Разрешаем только правдоподобные hostname/IPv4/IPv6 символы. Явно исключает ведущий
// "-" (иначе mtr в позиции target интерпретировал бы строку как флаг), пробелы,
// и любые метасимволы shell — хотя execFile и без экранирования не уязвим к shell
// injection, это defense-in-depth зеркалит подход firewall.service.ts (порты
// парсятся/проверяются по диапазону до использования).
const SAFE_TARGET_REGEX = /^[a-zA-Z0-9]([a-zA-Z0-9.:-]{0,252}[a-zA-Z0-9])?$/;

export function isValidDiagnosticTarget(target: unknown): target is string {
  if (typeof target !== 'string') return false;
  const trimmed = target.trim();
  if (trimmed.length === 0 || trimmed.length > 253) return false;
  if (trimmed.startsWith('-')) return false;
  return SAFE_TARGET_REGEX.test(trimmed);
}

interface ParsedMtrHop {
  hopNumber: number;
  address: string;
  lossPercent: number;
  avgLatencyMs: number;
}

interface ParsedMtrResult {
  reachable: boolean;
  lossPercent: number;
  avgLatencyMs: number;
  lossyHops: ParsedMtrHop[];
}

/**
 * Разбирает JSON-отчёт mtr (`mtr --report --json ...`) в плоскую структуру.
 *
 * Формат mtr JSON: { report: { hubs: [ { count, host, "Loss%", Avg, ... }, ... ] } }.
 * - `reachable`/`lossPercent`/`avgLatencyMs` берутся из ПОСЛЕДНЕГО хопа (реальная цель,
 *   а не промежуточный маршрутизатор); если его host — сентинел "???", цель считается
 *   недостижимой.
 * - `lossyHops` включает ТОЛЬКО хопы с Loss% > 0 — намеренное решение, чтобы не
 *   раздувать gRPC payload полным списком всех хопов на каждый вызов.
 */
export function parseMtrJsonReport(rawJson: string): ParsedMtrResult {
  let parsed: any;
  try {
    parsed = JSON.parse(rawJson);
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown JSON parse error';
    throw new Error(`Failed to parse mtr JSON output: ${msg}`);
  }

  const hubs = parsed?.report?.hubs;
  if (!Array.isArray(hubs) || hubs.length === 0) {
    throw new Error('mtr JSON report did not contain any hubs (hops).');
  }

  const lossyHops: ParsedMtrHop[] = [];
  for (const hub of hubs) {
    const lossPercent = Number(hub?.['Loss%']) || 0;
    if (lossPercent > 0) {
      lossyHops.push({
        hopNumber: Number(hub?.count) || 0,
        address: String(hub?.host ?? UNRESOLVED_HOST_SENTINEL),
        lossPercent,
        avgLatencyMs: Number(hub?.Avg) || 0
      });
    }
  }

  const lastHub = hubs[hubs.length - 1];
  const lastHost = String(lastHub?.host ?? '').trim();
  const reachable = lastHost.length > 0 && lastHost !== UNRESOLVED_HOST_SENTINEL;

  return {
    reachable,
    lossPercent: Number(lastHub?.['Loss%']) || 0,
    avgLatencyMs: Number(lastHub?.Avg) || 0,
    lossyHops
  };
}

function buildHopPayload(hop: ParsedMtrHop): any {
  return {
    hop_number: hop.hopNumber,
    hopNumber: hop.hopNumber,
    address: hop.address,
    loss_percent: hop.lossPercent,
    lossPercent: hop.lossPercent,
    avg_latency_ms: hop.avgLatencyMs,
    avgLatencyMs: hop.avgLatencyMs
  };
}

function buildTargetResultPayload(
  target: string,
  result: { reachable: boolean; lossPercent: number; avgLatencyMs: number; lossyHops: ParsedMtrHop[] },
  error: string
): any {
  const lossyHopsPayload = result.lossyHops.map(buildHopPayload);
  return {
    target,
    reachable: result.reachable,
    loss_percent: result.lossPercent,
    lossPercent: result.lossPercent,
    avg_latency_ms: result.avgLatencyMs,
    avgLatencyMs: result.avgLatencyMs,
    lossy_hops: lossyHopsPayload,
    lossyHops: lossyHopsPayload,
    error
  };
}

/**
 * Запускает `mtr` для одной цели и возвращает готовый payload NetworkDiagnosticTargetResult.
 * Никогда не бросает — любая ошибка (плохой hostname, mtr не установлен, таймаут,
 * невалидный JSON) превращается в { reachable: false, error: <message> } для ЭТОЙ
 * цели, чтобы одна плохая цель не ломала весь RPC-вызов.
 */
async function diagnoseTarget(target: string): Promise<any> {
  try {
    const { stdout } = await execFileAsync(
      'mtr',
      ['--report', '--json', '--no-dns', '--report-cycles', String(MTR_REPORT_CYCLES), target],
      { timeout: MTR_TIMEOUT_MS }
    );
    const parsed = parseMtrJsonReport(stdout);
    return buildTargetResultPayload(target, parsed, '');
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ target, err: msg }, 'mtr network diagnostic failed for target');
    return buildTargetResultPayload(target, { reachable: false, lossPercent: 0, avgLatencyMs: 0, lossyHops: [] }, msg);
  }
}

/**
 * RPC Обработчик RunNetworkDiagnostic
 *
 * Диагностический (read-only) RPC: оркестратор дважды в день опрашивает ограниченный
 * набор нод, чтобы выявить деградацию на общем сетевом участке (packet loss на общем
 * хопе у нескольких нод), НЕ для автоматической ротации/эвакуации ноды. Поэтому
 * `success` отражает "смогли ли мы попытаться обработать все цели", а не
 * "были ли все цели достижимы" — недостижимость конкретной цели не является
 * ошибкой самого RPC.
 */
export async function runNetworkDiagnosticHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized RunNetworkDiagnostic request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', results: [] });
  }

  const rawTargets = call.request?.targets;
  const targets: unknown[] = Array.isArray(rawTargets) ? rawTargets : [];

  // Run all targets concurrently, not sequentially — each mtr invocation can take up to
  // MTR_TIMEOUT_MS (45s) on its own; running N targets one after another (as this used to) made
  // the whole RPC take up to N * MTR_TIMEOUT_MS, which blew straight through the orchestrator's
  // own client-side deadline (GrpcNetworkDiagnosticClient.DEADLINE_MS) for anything beyond a
  // single target. Concurrent probing caps the wall-clock cost at ~MTR_TIMEOUT_MS regardless of
  // target count. Each target is still fully isolated (diagnoseTarget never throws), so
  // Promise.all here can't let one bad target take down the others.
  const results: any[] = await Promise.all(
    targets.map((rawTarget) => {
      if (!isValidDiagnosticTarget(rawTarget)) {
        const label = typeof rawTarget === 'string' ? rawTarget : String(rawTarget ?? '');
        logger.warn({ target: label }, 'Rejected invalid/unsafe RunNetworkDiagnostic target');
        return buildTargetResultPayload(
          label,
          { reachable: false, lossPercent: 0, avgLatencyMs: 0, lossyHops: [] },
          'Invalid or unsafe target string.'
        );
      }

      return diagnoseTarget(rawTarget.trim());
    })
  );

  const reachableCount = results.filter((r) => r.reachable).length;
  logger.info({ total: targets.length, reachable: reachableCount }, 'RunNetworkDiagnostic completed');

  return callback(null, {
    success: true,
    message: `Network diagnostic completed for ${targets.length} target(s): ${reachableCount} reachable, ${targets.length - reachableCount} unreachable/error.`,
    results
  });
}
