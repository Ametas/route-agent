import pino from 'pino';
import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import { authenticateCall } from '../middleware/auth.js';
import { collectAwgObservation } from '../utils/awgFlows.js';

const logger = pino({ level: 'info' });

/**
 * RPC-обработчик GetAwgObservation.
 *
 * Тонкий по делу: весь сбор и разбор живёт в `utils/awgFlows.ts` и проверяется без gRPC. Здесь
 * только авторизация и перевод в форму ответа.
 *
 * ПОЛЯ ОТВЕТА — camelCase. Сервер грузит proto с `keepCase: false`, и поле в snake_case
 * proto-loader молча ВЫБРАСЫВАЕТ: ни ошибки, ни предупреждения, оркестратор просто всегда видит
 * пустоту. На этом уже обожглись в `ConfigureRearSingbox`, теперь на класс стоит сторож
 * (`tests/responseFieldNaming.test.ts`).
 */
export async function getAwgObservationHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetAwgObservation request blocked');
    return callback(null, {
      success: false,
      message: 'Invalid orchestrator secret token.',
      peers: [],
      flows: [],
      conntrackAvailable: false,
    });
  }

  try {
    const observation = await collectAwgObservation();

    return callback(null, {
      success: true,
      message:
        observation.peers.length === 0
          ? 'AWG interface is not active on this node.'
          : `${observation.peers.length} peer(s), ${observation.flows.length} flow(s).`,
      peers: observation.peers.map((peer) => ({
        publicKey: peer.publicKey,
        tunnelIps: peer.tunnelIps,
        rxBytes: peer.rxBytes,
        txBytes: peer.txBytes,
        latestHandshakeUnix: peer.latestHandshakeUnix,
      })),
      flows: observation.flows.map((flow) => ({
        publicKey: flow.publicKey,
        tunnelIp: flow.tunnelIp,
        protocol: flow.protocol,
        destinationIp: flow.destinationIp,
        destinationPort: flow.destinationPort,
      })),
      conntrackAvailable: observation.conntrackAvailable,
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'GetAwgObservation failed');
    return callback(null, {
      success: false,
      message: msg,
      peers: [],
      flows: [],
      conntrackAvailable: false,
    });
  }
}
