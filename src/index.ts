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
import { applyConfigHandler, configureCaddyHandler, configureAwgHandler, getManagedCertificateHandler } from './services/config.service.js';
import {
  uploadSingboxBinaryHandler,
  uploadAwgToolsBinaryHandler,
  uploadAwgGoBinaryHandler,
  uploadCaddyBinaryHandler,
  upgradeSingboxHandler,
  sanitizeAwgToolsSymlink
} from './services/binary.service.js';
import { uploadMeshAwgKernelSourceHandler, configureMeshTunnelHandler } from './services/meshTunnel.service.js';
import {
  syncOlcrtcInstanceHandler,
  deleteOlcrtcInstanceHandler,
  streamOlcrtcEventsHandler,
  uploadOlcrtcAgentSrvBinaryHandler,
  ensureOlcrtcAgentSrvSystemdUnit
} from './services/olcrtc.service.js';
import { ensureAwgSystemdUnit } from './services/systemdUnit.service.js';
import { fixXraySocketPermissions } from './utils/caddy.js';
import { streamTelemetryHandler, getTelemetryHandler } from './services/telemetry.service.js';
import { startAwgHealthCheckTimer } from './utils/telemetry.js';
import { manageFirewallHandler } from './services/firewall.service.js';
import { selfUpdateHandler, getAgentInfoHandler } from './services/system.service.js';
import { runNetworkDiagnosticHandler } from './services/networkDiagnostic.service.js';
import { computeProtoContractHash } from './services/protoContract.service.js';
import { getSingBoxUserTrafficHandler } from './services/singboxStats.service.js';
import { getSingBoxConnectionsHandler } from './services/singboxConnections.service.js';
import { applyTrafficThrottleHandler } from './services/trafficThrottle.service.js';
import { configureRearSingboxHandler } from './services/rearSingbox.service.js';

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
 * Инициализация gRPC ServerCredentials со строгим mTLS.
 *
 * Security note: агент НЕ ИМЕЕТ insecure-фоллбека. Если CA/agent cert/agent key
 * отсутствуют, недоступны для чтения, или их парсинг завершился ошибкой — функция
 * бросает исключение и агент не стартует. Это осознанное решение: тихая деградация
 * в ServerCredentials.createInsecure() означала, что x-orchestrator-secret уходил
 * бы в открытом виде на каждый вызов (MITM => компрометация + replay секрета через
 * полноценно авторизованное соединение). См. checkServerIdentity на стороне
 * оркестратора — тот же принцип "fail closed", применённый здесь симметрично.
 */
export async function getGrpcServerCredentials(): Promise<ServerCredentials> {
  const { CA_CERT_PATH, AGENT_CERT_PATH, AGENT_KEY_PATH } = config;
  const [caExists, certExists, keyExists] = await Promise.all([
    fs.stat(CA_CERT_PATH).then(() => true).catch(() => false),
    fs.stat(AGENT_CERT_PATH).then(() => true).catch(() => false),
    fs.stat(AGENT_KEY_PATH).then(() => true).catch(() => false),
  ]);

  if (caExists && certExists && keyExists) {
    try {
      const [caCert, agentCert, agentKey] = await Promise.all([
        fs.readFile(CA_CERT_PATH),
        fs.readFile(AGENT_CERT_PATH),
        fs.readFile(AGENT_KEY_PATH),
      ]);

      logger.info({ caPath: CA_CERT_PATH, certPath: AGENT_CERT_PATH }, '🔒 Enabling mTLS with strict client certificate verification');
      return ServerCredentials.createSsl(caCert, [{ cert_chain: agentCert, private_key: agentKey }], true);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      throw new Error(
        `mTLS certificates were found but failed to load (CA: ${CA_CERT_PATH}, cert: ${AGENT_CERT_PATH}, ` +
        `key: ${AGENT_KEY_PATH}). Underlying error: ${msg}. Agent refuses to start in an unencrypted/` +
        `unauthenticated transport mode. Verify the certificate/key files are valid PEM material issued for ` +
        `this node, or reissue them via the orchestrator install flow (re-run install.sh with a valid ` +
        `--secret), then restart the route-agent service.`
      );
    }
  }

  throw new Error(
    `mTLS certificates are required but unavailable ` +
    `(CA: ${caExists ? 'ok' : `MISSING at ${CA_CERT_PATH}`}, ` +
    `cert: ${certExists ? 'ok' : `MISSING at ${AGENT_CERT_PATH}`}, ` +
    `key: ${keyExists ? 'ok' : `MISSING at ${AGENT_KEY_PATH}`}). ` +
    `Agent refuses to start in an unencrypted/unauthenticated transport mode instead of silently degrading ` +
    `(x-orchestrator-secret must never travel in plaintext). Reissue node certificates via the orchestrator ` +
    `install flow (re-run install.sh on this VPS with a valid --secret to re-enroll and receive fresh CA/` +
    `agent cert+key), or manually place them at the CA_CERT_PATH/AGENT_CERT_PATH/AGENT_KEY_PATH configured ` +
    `in .env, then restart the route-agent service.`
  );
}

export async function startServer(): Promise<Server> {
  await sanitizeAwgToolsSymlink();
  await ensureAwgSystemdUnit();
  await ensureOlcrtcAgentSrvSystemdUnit();
  await fixXraySocketPermissions();
  startAwgHealthCheckTimer();
  const credentials = await getGrpcServerCredentials();
  return new Promise((resolve, reject) => {
    const serverOptions = {
      'grpc.keepalive_time_ms': 10000,
      'grpc.keepalive_timeout_ms': 5000,
      'grpc.keepalive_permit_without_calls': 1,
      'grpc.http2.min_ping_interval_without_data_ms': 5000,
      'grpc.http2.max_pings_without_data': 0,
    };
    const server = new Server(serverOptions);
    const serviceImplementation: UntypedServiceImplementation = {
      applyConfig: applyConfigHandler,
      streamTelemetry: streamTelemetryHandler,
      getTelemetry: getTelemetryHandler,
      upgradeSingbox: upgradeSingboxHandler,
      uploadSingboxBinary: uploadSingboxBinaryHandler,
      uploadOlcrtcAgentSrvBinary: uploadOlcrtcAgentSrvBinaryHandler,
      uploadAwgToolsBinary: uploadAwgToolsBinaryHandler,
      uploadAwgGoBinary: uploadAwgGoBinaryHandler,
      uploadCaddyBinary: uploadCaddyBinaryHandler,
      configureCaddy: configureCaddyHandler,
      syncOlcrtcInstance: syncOlcrtcInstanceHandler,
      deleteOlcrtcInstance: deleteOlcrtcInstanceHandler,
      streamOlcrtcEvents: streamOlcrtcEventsHandler,
      configureAwg: configureAwgHandler,
      uploadMeshAwgKernelSource: uploadMeshAwgKernelSourceHandler,
      configureMeshTunnel: configureMeshTunnelHandler,
      manageFirewall: manageFirewallHandler,
      selfUpdate: selfUpdateHandler,
      getAgentInfo: getAgentInfoHandler,
      runNetworkDiagnostic: runNetworkDiagnosticHandler,
      getManagedCertificate: getManagedCertificateHandler,
      getSingBoxUserTraffic: getSingBoxUserTrafficHandler,
      getSingBoxConnections: getSingBoxConnectionsHandler,
      applyTrafficThrottle: applyTrafficThrottleHandler,
      configureRearSingbox: configureRearSingboxHandler
    };
    
    server.addService(agentPackage.EgressAgentService.service, serviceImplementation);

    const bindTarget = `${config.HOST}:${config.PORT}`;
    server.bindAsync(bindTarget, credentials, (err, port) => {
      if (err) {
        logger.error({ err }, 'Failed to bind gRPC server');
        reject(err);
        return;
      }
      const protoHash = computeProtoContractHash();
      logger.info({ protoHash, len: protoHash.length }, `🚀 gRPC Route Agent actively listening at h2://${config.HOST}:${port} (Proto Contract SHA256: ${protoHash})`);
      serverInstance = server;
      resolve(server);
    });
  });
}

if (process.env.NODE_ENV !== 'test') {
  startServer().catch((err: unknown) => {
    const msg = err instanceof Error ? err.message : String(err);
    logger.fatal({ err: msg }, '💥 Route Agent failed to start and is exiting; systemd (Restart=always) will retry, but this will loop until the underlying issue is fixed');
    process.exit(1);
  });
}
