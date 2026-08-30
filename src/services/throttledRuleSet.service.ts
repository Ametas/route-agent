import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { authenticateCall } from '../middleware/auth.js';
import { writeThrottledRuleSet } from '../utils/throttledRuleSet.js';

const logger = pino({ level: 'info' });

/**
 * RPC handler for ApplyThrottledRuleSet — see proto/agent.proto and utils/throttledRuleSet.ts for
 * the full rationale (local file rather than a remote rule-set, so the orchestrator never becomes
 * a boot-time dependency of a node).
 *
 * Never throws past this function, same posture as every other handler here. A rejected document
 * is reported as a failure rather than written: a malformed rule-set file would be picked up by
 * fswatch and logged as a reload error by sing-box, leaving the node with a stale list and no
 * obvious cause.
 */
export async function applyThrottledRuleSetHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized ApplyThrottledRuleSet request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', changed: false });
  }

  const contentJson = call.request?.contentJson || call.request?.content_json;
  if (typeof contentJson !== 'string' || contentJson.trim() === '') {
    return callback(null, { success: false, message: 'Empty rule-set document', changed: false });
  }

  try {
    const { changed } = await writeThrottledRuleSet(contentJson);
    if (changed) {
      logger.info('Throttled-ranges rule-set updated');
    }
    return callback(null, { success: true, message: changed ? 'Updated' : 'Unchanged', changed });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg }, 'Failed to write the throttled-ranges rule-set');
    return callback(null, { success: false, message: msg, changed: false });
  }
}
