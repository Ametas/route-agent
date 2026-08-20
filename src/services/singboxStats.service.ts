import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { authenticateCall } from '../middleware/auth.js';
import { readLiveV2RayApiListenAddress, queryUserTrafficDeltas } from '../utils/singboxStats.js';

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
    const listenAddress = await readLiveV2RayApiListenAddress();
    if (!listenAddress) {
      // Not an error — a node with no tuic/hy2Sb configured (or not yet re-pushed with the
      // v2ray_api block) legitimately has nothing to report here.
      return callback(null, { success: true, message: 'v2ray_api not configured on this node', entries: [] });
    }

    const deltas = await queryUserTrafficDeltas(listenAddress);
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
