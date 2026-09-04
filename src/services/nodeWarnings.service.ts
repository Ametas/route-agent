import pino from 'pino';
import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import { config } from '../config.js';
import { authenticateCall } from '../middleware/auth.js';
import { collectJournalWarnings } from '../utils/journalWarnings.js';
import { detectCoreNotListening } from '../utils/listenerWarning.js';

const logger = pino({ level: 'info' });

/**
 * RPC-обработчик PullNodeWarnings.
 *
 * Тонкий по делу: весь разбор живёт в `utils/journalWarnings.ts` и проверяется без gRPC. Здесь
 * только авторизация и перевод в форму ответа.
 *
 * ПОЛЯ ОТВЕТА — camelCase. Сервер грузит proto с `keepCase: false`, и поле в snake_case
 * proto-loader молча ВЫБРАСЫВАЕТ: ни ошибки, ни предупреждения, оркестратор просто всегда видит
 * пустоту. На этом уже обожглись в `ConfigureRearSingbox`, теперь на класс стоит сторож
 * (`tests/responseFieldNaming.test.ts`).
 */
export async function pullNodeWarningsHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized PullNodeWarnings request blocked');
    return callback(null, {
      success: false,
      message: 'Invalid orchestrator secret token.',
      warnings: [],
      lookbackHours: 0,
    });
  }

  try {
    /**
     * Два источника, один доклад.
     *
     * Журнал ловит то, что процесс СКАЗАЛ; проверка сокетов — то, о чём он молчит. Нода, три недели
     * простоявшая без единого слушателя на публичном порту, не написала об этом ни строчки: она
     * просто ничего не обслуживала. Грепом отсутствие события не найти.
     */
    const [journal, notListening] = await Promise.all([collectJournalWarnings(), detectCoreNotListening()]);
    const warnings = notListening ? [notListening, ...journal] : journal;

    if (warnings.length > 0) {
      logger.info(
        { kinds: warnings.map((w) => `${w.kind}@${w.source}`) },
        'Journal warnings found and reported to the orchestrator'
      );
    }

    return callback(null, {
      success: true,
      message: warnings.length === 0 ? 'No degradation warnings in the journal.' : `${warnings.length} warning class(es) found.`,
      warnings: warnings.map((w) => ({
        kind: w.kind,
        source: w.source,
        sample: w.sample,
        occurrences: w.occurrences,
        lastSeenUnixMs: w.lastSeenUnixMs,
      })),
      lookbackHours: config.NODE_WARNING_LOOKBACK_HOURS,
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to collect journal warnings');
    return callback(null, {
      success: false,
      message: `Internal Agent Error: ${msg}`,
      warnings: [],
      lookbackHours: config.NODE_WARNING_LOOKBACK_HOURS,
    });
  }
}
