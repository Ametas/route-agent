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

  const raw: unknown = call.request?.entries;
  const entries = (Array.isArray(raw) ? raw : [])
    .map((e: any) => ({
      prefix: typeof e?.prefix === 'string' ? e.prefix.trim() : '',
      rate: typeof e?.rate === 'string' ? e.rate.trim() : '',
      burst: typeof e?.burst === 'string' ? e.burst.trim() : '',
    }))
    .filter((e) => e.prefix !== '');

  try {
    const counters = await readThrottleCounters();

    if (entries.length === 0) {
      await clearThrottle();
      return callback(null, { success: true, message: 'Cleared', changed: true, applied: 0, counters });
    }

    const incomplete = entries.filter((e) => !e.rate || !e.burst);
    if (incomplete.length > 0) {
      // Не подставляем «разумные» значения за оркестратора: придушить кого-то наугад хуже, чем
      // отказаться и сказать об этом вслух.
      return callback(null, {
        success: false,
        message: `Entries without rate/burst: ${incomplete.map((e) => e.prefix).join(', ')}`,
        changed: false,
        applied: 0,
        counters,
      });
    }

    const result = await applyThrottle(entries);
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
