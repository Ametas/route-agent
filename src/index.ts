import { 
  Server, 
  ServerCredentials, 
  UntypedServiceImplementation,
  loadPackageDefinition
} from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import { config } from './config.js';
import { applyConfigHandler, configureCaddyHandler, configureAwgHandler, configureOlcrtcHandler } from './services/config.service.js';
import { uploadSingboxBinaryHandler, uploadOlcrtcBinaryHandler, upgradeSingboxHandler } from './services/binary.service.js';
import { streamTelemetryHandler } from './services/telemetry.service.js';
import { manageFirewallHandler } from './services/firewall.service.js';
import { selfUpdateHandler } from './services/system.service.js';

const logger = pino({ level: 'info' });

const PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');
const packageDefinition = protoLoader.loadSync(PROTO_PATH, {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: true,
  oneofs: true,
});

const protoDescriptor = loadPackageDefinition(packageDefinition) as any;
const agentPackage = protoDescriptor.agent;

export let serverInstance: Server | null = null;

/**
 * Инициализация gRPC ServerCredentials (mTLS с проверкой сертификатов или insecure fallback)
 */
async function getGrpcServerCredentials(): Promise<{ credentials: ServerCredentials; isMtls: boolean }> {
  try {
    const { CA_CERT_PATH, AGENT_CERT_PATH, AGENT_KEY_PATH } = config;
    const [caExists, certExists, keyExists] = await Promise.all([
      fs.stat(CA_CERT_PATH).then(() => true).catch(() => false),
      fs.stat(AGENT_CERT_PATH).then(() => true).catch(() => false),
      fs.stat(AGENT_KEY_PATH).then(() => true).catch(() => false),
    ]);

    if (caExists && certExists && keyExists) {
      const [caCert, agentCert, agentKey] = await Promise.all([
        fs.readFile(CA_CERT_PATH),
        fs.readFile(AGENT_CERT_PATH),
        fs.readFile(AGENT_KEY_PATH),
      ]);

      logger.info({ caPath: CA_CERT_PATH, certPath: AGENT_CERT_PATH }, '🔒 Enabling mTLS with strict client certificate verification');
      return {
        credentials: ServerCredentials.createSsl(caCert, [{ cert_chain: agentCert, private_key: agentKey }], true),
        isMtls: true
      };
    }
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.warn({ err: msg }, 'Failed to load mTLS certificates; falling back to insecure gRPC channel');
  }

  logger.warn('⚠️ mTLS certificates not found or incomplete. Starting gRPC server in insecure mode');
  return { credentials: ServerCredentials.createInsecure(), isMtls: false };
}

export async function startServer(): Promise<Server> {
  const { credentials, isMtls } = await getGrpcServerCredentials();
  return new Promise((resolve, reject) => {
    const server = new Server();
    const serviceImplementation: UntypedServiceImplementation = {
      applyConfig: applyConfigHandler,
      streamTelemetry: streamTelemetryHandler,
      upgradeSingbox: upgradeSingboxHandler,
      uploadSingboxBinary: uploadSingboxBinaryHandler,
      uploadOlcrtcBinary: uploadOlcrtcBinaryHandler,
      configureCaddy: configureCaddyHandler,
      configureOlcrtc: configureOlcrtcHandler,
      configureAwg: configureAwgHandler,
      manageFirewall: manageFirewallHandler,
      selfUpdate: selfUpdateHandler
    };
    
    server.addService(agentPackage.EgressAgentService.service, serviceImplementation);

    const bindTarget = `${config.HOST}:${config.PORT}`;
    server.bindAsync(bindTarget, credentials, (err, port) => {
      if (err) {
        logger.error({ err }, 'Failed to bind gRPC server');
        reject(err);
        return;
      }
      const scheme = isMtls ? 'h2' : 'h2c';
      logger.info(`🚀 gRPC Route Agent actively listening at ${scheme}://${config.HOST}:${port}`);
      serverInstance = server;
      resolve(server);
    });
  });
}

if (process.env.NODE_ENV !== 'test') {
  startServer().catch(() => {
    process.exit(1);
  });
}
