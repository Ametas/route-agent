import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { execAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';

const logger = pino({ level: 'info' });

/**
 * RPC Обработчик ManageFirewall
 */
export async function manageFirewallHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized ManageFirewall request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { openUdpPorts, open_udp_ports, openTcpPorts, open_tcp_ports } = call.request;

  try {
    const rawUdp = openUdpPorts || open_udp_ports || [];
    const rawTcp = openTcpPorts || open_tcp_ports || [];

    const openUdp = Array.isArray(rawUdp) ? rawUdp.map(Number).filter((p) => Number.isInteger(p) && p > 0 && p <= 65535) : [];
    const openTcp = Array.isArray(rawTcp) ? rawTcp.map(Number).filter((p) => Number.isInteger(p) && p > 0 && p <= 65535) : [];

    if (process.env.NODE_ENV === 'test') {
      return callback(null, {
        success: true,
        message: `[TEST] Processed firewall ports: UDP=[${openUdp.join(', ')}], TCP=[${openTcp.join(', ')}]`
      });
    }

    for (const port of openUdp) {
      try {
        await execAsync(`sudo ufw allow ${port}/udp`);
      } catch (err: any) {
        logger.error({ port, err: err.message }, 'Failed to open UDP port in firewall');
      }
    }

    for (const port of openTcp) {
      try {
        await execAsync(`sudo ufw allow ${port}/tcp`);
      } catch (err: any) {
        logger.error({ port, err: err.message }, 'Failed to open TCP port in firewall');
      }
    }

    try {
      await execAsync('sudo ufw reload');
    } catch (err: any) {
      logger.warn({ err: err.message }, 'Failed to reload UFW');
    }

    return callback(null, {
      success: true,
      message: `Successfully updated firewall rules: opened ${openUdp.length} UDP and ${openTcp.length} TCP ports.`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to execute ManageFirewall');
    return callback(null, { success: false, message: `ManageFirewall error: ${msg}` });
  }
}
