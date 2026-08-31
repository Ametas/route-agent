import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { authenticateCall } from '../middleware/auth.js';
import { queryUserTrafficDeltas } from '../utils/singboxStats.js';

const logger = pino({ level: 'info' });

/**
 * RPC handler for GetSingBoxUserTraffic — see proto/agent.proto's own doc comment on this RPC for
 * the full "Track B-lite" rationale. Never throws past this function: any failure (v2ray_api not
 * configured, sing-box unreachable, malformed response) comes back as `{ success: false, message,
 * entries: [] }`, same "degrade, never crash the RPC" posture as every other handler in this repo.
 */
export async function getSingBoxUserTrafficHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetSingBoxUserTraffic request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', entries: [] });
  }

  try {
    const deltas = await queryUserTrafficDeltas();
    if (deltas === null) {
      // Не ошибка: на ноду ещё не приезжал конфиг с блоком clash_api, спрашивать негде.
      //
      // Отвечаем УСПЕХОМ с пустым списком — и это ровно та форма, из-за которой предыдущая версия
      // модуля молча притворялась рабочей полгода: она попадала сюда ВСЕГДА, потому что искала
      // блок, который оркестратор давно перестал эмитить. Сама форма ответа правильная (нода без
      // конфига — не авария), опасна она тем, что неотличима от настоящей поломки. Поэтому текст
      // сообщения называет конкретную причину, а не «не настроено»: если это снова начнёт
      // приходить со всего флота, по строке будет видно, что именно не найдено.
      return callback(null, { success: true, message: 'clash_api block absent from the applied sing-box config', entries: [] });
    }
    return callback(null, {
      success: true,
      message: 'OK',
      entries: deltas.map((d) => ({
        userUuid: d.userUuid,
        uplinkBytes: d.uplinkBytes,
        downlinkBytes: d.downlinkBytes,
      })),
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg }, 'Failed to fetch sing-box per-user traffic stats');
    return callback(null, { success: false, message: msg, entries: [] });
  }
}
