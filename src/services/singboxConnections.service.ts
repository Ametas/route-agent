import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { authenticateCall } from '../middleware/auth.js';
import { readClashApiEndpoint, fetchConnections } from '../utils/singboxConnections.js';
import { collectRearConnections } from '../utils/rearConnections.js';

const logger = pino({ level: 'info' });

/**
 * RPC handler for GetSingBoxConnections — see proto/agent.proto's own doc comment for the full
 * rationale, and utils/singboxConnections.ts for why this replaced the v2ray_api channel.
 *
 * Never throws past this function: any failure (clash_api not configured, sing-box unreachable,
 * malformed response) comes back as a populated response object, same "degrade, never crash the
 * RPC" posture as every other handler in this repo.
 *
 * The same handler answers on egress nodes and on Xeon fronts — one agent runs on both. It makes
 * no attempt to interpret what it reads: whether `user` or `sourceIp` is the meaningful field
 * here depends on which side this node sits on, and that judgement belongs to the orchestrator.
 */
export async function getSingBoxConnectionsHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetSingBoxConnections request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', connections: [] });
  }

  try {
    const endpoint = await readClashApiEndpoint();
    if (!endpoint) {
      // Not an error — a node whose config predates the clash_api block simply has nothing to
      // report yet. Reported as success so a sweep over the fleet does not treat "not rolled out
      // here" the same as "this node is broken".
      return callback(null, { success: true, message: 'clash_api not configured on this node', connections: [] });
    }

    const connections = await fetchConnections(endpoint);
    return callback(null, { success: true, message: 'OK', connections });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg }, 'Failed to fetch sing-box connections');
    return callback(null, { success: false, message: msg, connections: [] });
  }
}

/**
 * RPC handler for GetRearConnections — соединения ТЫЛОВОГО ядра.
 *
 * Отдельный обработчик рядом с фронтовым, а не флаг в том же: источники разные (Clash API sing-box
 * против Clash API mihomo), формы ответа разные, и «тыла на узле нет» — штатное состояние, тогда
 * как отсутствие фронтового ядра означает совсем другое. Общей у них остаётся только форма
 * ОТВЕТА, и она вынесена в общий тип записи.
 *
 * Как и сосед: наружу не бросает, отказ приезжает заполненным ответом.
 */
export async function getRearConnectionsHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetRearConnections request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.', connections: [] });
  }

  try {
    const connections = await collectRearConnections();
    return callback(null, {
      success: true,
      message: connections.length === 0 ? 'Rear core is not active on this node.' : 'OK',
      connections,
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg }, 'Failed to fetch rear connections');
    return callback(null, { success: false, message: msg, connections: [] });
  }
}
