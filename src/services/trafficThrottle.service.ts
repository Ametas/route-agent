import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { authenticateCall } from '../middleware/auth.js';
import { applyThrottle, clearThrottle, readThrottleCounters } from '../utils/trafficShaper.js';

const logger = pino({ level: 'info' });

/**
 * RPC handler for ApplyTrafficThrottle — see proto/agent.proto for the rationale and
 * utils/trafficShaper.ts for the mechanism.
 *
 * Counters are read FIRST, before anything is changed: rebuilding the tc filters resets them, and
 * they are the evidence the orchestrator's escalation decision rests on. Reading them after would
 * quietly return zeros on every delivery that changed something.
 *
 * Never throws past this function, same posture as every other handler here.
 */
export async function applyTrafficThrottleHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized ApplyTrafficThrottle request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', changed: false, applied: 0, counters: [] });
  }

  const prefixes: string[] = Array.isArray(call.request?.prefixes) ? call.request.prefixes : [];
  const rate = typeof call.request?.rate === 'string' ? call.request.rate.trim() : '';
  const burst = typeof call.request?.burst === 'string' ? call.request.burst.trim() : '';

  try {
    const counters = await readThrottleCounters();

    if (prefixes.length === 0) {
      await clearThrottle();
      return callback(null, { success: true, message: 'Cleared', changed: true, applied: 0, counters });
    }

    if (!rate || !burst) {
      // Отказываемся молча не ставить ограничение: без обеих величин полисер бессмыслен, а
      // подставить «разумное» значение за оркестратора значило бы придушить кого-то наугад.
      return callback(null, { success: false, message: 'Both rate and burst are required', changed: false, applied: 0, counters });
    }

    const result = await applyThrottle(prefixes, rate, burst);
    return callback(null, {
      success: true,
      message: result.message,
      changed: result.changed,
      applied: result.applied,
      counters,
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg }, 'Failed to apply the traffic throttle');
    return callback(null, { success: false, message: msg, changed: false, applied: 0, counters: [] });
  }
}
