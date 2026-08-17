import { ServerReadableStream, ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import * as fs from 'fs/promises';
import { createWriteStream } from 'fs';
import path from 'path';
import os from 'os';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';
import { getMeshAwgInterfaceName } from '../utils/awg.js';
import { invalidateMeshAwgKernelStatusCache } from '../utils/telemetry.js';

const logger = pino({ level: 'info' });

function sanitizeConfigInput(val: string | number | undefined | null): string {
  if (val === undefined || val === null) return '';
  return String(val).replace(/[\r\n]/g, '').trim();
}

// --- S2S AWG3 mesh tunnel (front↔egress, second ring-relay hop): kernel-module install --------
// See план "S2S AWG3-туннель фронт↔egress", Часть 7.1 — the source tarball is fetched centrally
// by the orchestrator (githubService.ts's ensureAwgKernelModuleSource, never git-cloned by this
// RF-hosted node directly) and streamed here via the same BinaryChunkPayload envelope every other
// Upload*Binary RPC uses. Unlike those, the payload is SOURCE, not a prebuilt binary — a kernel
// module can't be cross-compiled centrally, it must be built against this node's own running
// kernel via DKMS.

const DKMS_MODULE_FALLBACK_NAME = 'amneziawg';
const DKMS_MODULE_FALLBACK_VERSION = '1.0.0';

interface DkmsPackageInfo {
  name: string;
  version: string;
  srcDir: string; // directory containing dkms.conf itself (the `make dkms-install` cwd)
}

/**
 * Real upstream layout (verified against amnezia-vpn/amneziawg-linux-kernel-module): dkms.conf
 * lives at `<repo>/src/dkms.conf`. Falls back to a shallow (2-level) search in case that ever
 * changes, rather than hardcoding the single path with no recovery.
 */
async function findDkmsConf(buildDir: string): Promise<string> {
  const candidates = [path.join(buildDir, 'src', 'dkms.conf'), path.join(buildDir, 'dkms.conf')];
  for (const candidate of candidates) {
    try {
      await fs.access(candidate);
      return candidate;
    } catch {}
  }

  const entries = await fs.readdir(buildDir, { withFileTypes: true }).catch(() => []);
  for (const entry of entries) {
    if (!entry.isDirectory()) continue;
    const nested = path.join(buildDir, entry.name, 'dkms.conf');
    try {
      await fs.access(nested);
      return nested;
    } catch {}
  }

  throw new Error(`dkms.conf not found in extracted source tarball under ${buildDir}`);
}

async function parseDkmsConf(dkmsConfPath: string): Promise<DkmsPackageInfo> {
  const content = await fs.readFile(dkmsConfPath, 'utf-8');
  const nameMatch = content.match(/PACKAGE_NAME\s*=\s*"([^"]+)"/);
  const versionMatch = content.match(/PACKAGE_VERSION\s*=\s*"([^"]+)"/);

  if (!nameMatch || !versionMatch) {
    logger.warn({ dkmsConfPath }, 'Could not parse PACKAGE_NAME/PACKAGE_VERSION from dkms.conf, using fallback values');
  }

  return {
    name: nameMatch ? nameMatch[1] : DKMS_MODULE_FALLBACK_NAME,
    version: versionMatch ? versionMatch[1] : DKMS_MODULE_FALLBACK_VERSION,
    srcDir: path.dirname(dkmsConfPath),
  };
}

async function isDkmsModuleInstalled(name: string, version: string): Promise<boolean> {
  try {
    const { stdout } = await execAsync(`dkms status -m ${name} -v ${version}`);
    return /installed/i.test(stdout);
  } catch {
    return false;
  }
}

/**
 * DKMS build failures land a log at /var/lib/dkms/<pkg>/<ver>/build/make.log — surfacing its tail
 * in the RPC response gives the admin actionable diagnostic detail in Telegram without needing to
 * SSH in first, matching this project's existing diagnostic-rich error style.
 */
async function readDkmsBuildLogTail(name: string, version: string): Promise<string> {
  const logPath = `/var/lib/dkms/${name}/${version}/build/make.log`;
  try {
    const content = await fs.readFile(logPath, 'utf-8');
    return content.length > 2000 ? content.slice(-2000) : content;
  } catch {
    return '';
  }
}

async function installDkmsKernelModule(buildDir: string): Promise<{ name: string; version: string }> {
  const dkmsConfPath = await findDkmsConf(buildDir);
  const pkg = await parseDkmsConf(dkmsConfPath);

  const alreadyInstalled = await isDkmsModuleInstalled(pkg.name, pkg.version);
  if (alreadyInstalled) {
    logger.info({ name: pkg.name, version: pkg.version }, 'DKMS module already installed for this version, skipping rebuild');
  } else if (process.env.NODE_ENV !== 'test') {
    logger.info({ name: pkg.name, version: pkg.version }, 'Installing DKMS build dependencies (dkms/build-essential/linux-headers)...');
    await execAsync('apt-get update -y', { timeout: 120000 });
    await execAsync('apt-get install -y dkms build-essential linux-headers-$(uname -r)', { timeout: 300000 });

    await execAsync(`make -C "${pkg.srcDir}" dkms-install`, { timeout: 60000 });

    try {
      await execAsync(`dkms add -m ${pkg.name} -v ${pkg.version}`, { timeout: 30000 });
    } catch (err: any) {
      const out = (err.stderr || err.stdout || err.message || '').toLowerCase();
      // "already added" is expected/idempotent on a re-run against a source tree that was left
      // behind by a previous attempt (e.g. isDkmsModuleInstalled said false because build/install
      // failed after add succeeded) — anything else is a genuine failure.
      if (!out.includes('already') || !out.includes('add')) {
        throw err;
      }
      logger.info({ name: pkg.name, version: pkg.version }, 'DKMS module was already added (previous partial attempt), continuing to build/install');
    }

    try {
      await execAsync(`dkms build -m ${pkg.name} -v ${pkg.version}`, { timeout: 180000 });
      await execAsync(`dkms install -m ${pkg.name} -v ${pkg.version}`, { timeout: 60000 });
    } catch (buildErr: any) {
      const logTail = await readDkmsBuildLogTail(pkg.name, pkg.version);
      const baseMsg = buildErr instanceof Error ? buildErr.message : String(buildErr);
      throw new Error(logTail ? `${baseMsg}\n\n--- dkms build log (tail) ---\n${logTail}` : baseMsg);
    }
  }

  if (process.env.NODE_ENV !== 'test') {
    await execAsync('modprobe amneziawg', { timeout: 15000 });
    await execAsync('lsmod | grep -w amneziawg', { timeout: 5000 });

    // Persist across reboots via the standard systemd-modules-load mechanism — DKMS's own
    // AUTOINSTALL=yes (see dkms.conf) rebuilds the module against a NEW kernel on upgrade, but
    // doesn't by itself guarantee it gets modprobe'd back into the running kernel at every boot.
    await fs.mkdir('/etc/modules-load.d', { recursive: true }).catch(() => {});
    await fs.writeFile('/etc/modules-load.d/amneziawg.conf', 'amneziawg\n', 'utf-8').catch((err) => {
      logger.warn({ err: err.message }, 'Failed to write /etc/modules-load.d/amneziawg.conf (boot-persistence best-effort)');
    });
  }

  invalidateMeshAwgKernelStatusCache();
  return { name: pkg.name, version: pkg.version };
}

/**
 * RPC Обработчик UploadMeshAwgKernelSource (клиентский стрим RPC для загрузки исходников
 * kernel-модуля AmneziaWG — см. комментарий выше файла).
 */
export async function uploadMeshAwgKernelSourceHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  // os.tmpdir() (not a hardcoded '/tmp/...' literal, unlike the rest of this codebase's binary
  // upload handlers) — those never hand their temp path to an EXTERNAL process, only to Node's
  // own fs calls, which resolve a literal '/tmp/...' consistently by itself. This handler DOES
  // shell out (tar, dkms) to the same path, and on non-Linux dev/test boxes the external tool's
  // path resolution can disagree with Node's — os.tmpdir() keeps both sides consistent
  // everywhere, and is simply '/tmp' on the real Linux production targets this agent runs on.
  const tarPath = path.join(os.tmpdir(), `amneziawg_kernel_src_${uniqueId}.tar.gz`);
  let bytesWritten = 0;
  let fileStream: ReturnType<typeof createWriteStream> | null = null;

  call.on('data', (data: any) => {
    if (isAborted) return;

    if (!secretVerified) {
      if (verifySecret(data?.orchestratorSecret || data?.orchestrator_secret)) {
        secretVerified = true;
      }
    }

    if (!secretVerified) {
      isAborted = true;
      logger.warn('Unauthorized UploadMeshAwgKernelSource attempt rejected');
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (data.chunk && data.chunk.length > 0) {
      if (!fileStream) {
        fileStream = createWriteStream(tarPath);
      }
      const buf = Buffer.from(data.chunk);
      fileStream.write(buf);
      bytesWritten += buf.length;
    }
  });

  call.on('end', async () => {
    if (isAborted) return;

    if (fileStream) {
      await new Promise<void>((resolve) => fileStream!.end(resolve));
    }

    if (!secretVerified || bytesWritten === 0) {
      await fs.unlink(tarPath).catch(() => {});
      return callback(null, { success: false, message: 'No valid source data received or unauthorized.' });
    }

    const buildDir = path.join(os.tmpdir(), `amneziawg_kernel_build_${uniqueId}`);
    try {
      await fs.mkdir(buildDir, { recursive: true });
      await execAsync(`tar -xzf "${tarPath}" --strip-components=1 -C "${buildDir}"`, { timeout: 60000 });
      await fs.unlink(tarPath).catch(() => {});

      const { name, version } = await installDkmsKernelModule(buildDir);

      logger.info({ name, version }, 'AmneziaWG kernel module built and loaded via DKMS');
      return callback(null, {
        success: true,
        message: `AmneziaWG kernel module (${name} v${version}) installed and loaded`
      });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err);
      logger.error({ err: msg }, 'Failed to install AmneziaWG kernel module via DKMS');
      return callback(null, { success: false, message: `Failed to install kernel module: ${msg}` });
    } finally {
      await fs.unlink(tarPath).catch(() => {});
      await fs.rm(buildDir, { recursive: true, force: true }).catch(() => {});
    }
  });

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tarPath).catch(() => {});
  };

  call.on('error', (err) => {
    logger.error({ err: err.message }, 'Error in UploadMeshAwgKernelSource stream');
    cleanup();
  });
  call.on('cancelled', cleanup);
}

// --- S2S AWG3 mesh tunnel: interface configuration ---------------------------------------------
// Distinct interface/config from ConfigureAwg (client-facing AWG, config.service.ts) — own key
// material, own peer set, own config file (MESH_AWG_CONFIG_PATH). Reuses the same generic
// route-awg@.service template unit (%i-parameterized, provisioned once at agent startup via
// ensureAwgSystemdUnit — see index.ts) that ConfigureAwg already relies on, just targeting a
// different interface name.

/**
 * RPC Обработчик ConfigureMeshTunnel — пишет /etc/amnezia/amneziawg/<iface>.conf и поднимает/
 * перезагружает интерфейс. `systemctl reload route-awg@<iface>` исполняет юнитовый ExecReload
 * (`awg syncconf %i <(awg-quick strip %i)`) — живой пересинк peer-листа без обрыва уже
 * установленных сессий остальных peer'ов, если интерфейс уже поднят; `start` только при первом
 * поднятии.
 */
export async function configureMeshTunnelHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized ConfigureMeshTunnel request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { privateKey, addressV4, addressV6, listenPort, jc, jmin, jmax, peers } = call.request;

  try {
    const meshConfigPath = config.MESH_AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/awgmesh0.conf';
    const iface = getMeshAwgInterfaceName();
    const unitName = `route-awg@${iface}`;

    const cleanPrivateKey = sanitizeConfigInput(privateKey);
    const cleanAddressV4 = sanitizeConfigInput(addressV4);
    const cleanAddressV6 = sanitizeConfigInput(addressV6);

    const addresses: string[] = [];
    if (cleanAddressV4) addresses.push(cleanAddressV4);
    if (cleanAddressV6) addresses.push(cleanAddressV6);

    let configContent = '[Interface]\n';
    if (cleanPrivateKey) configContent += `PrivateKey = ${cleanPrivateKey}\n`;
    if (listenPort) configContent += `ListenPort = ${listenPort}\n`;
    if (addresses.length > 0) configContent += `Address = ${addresses.join(', ')}\n`;
    if (jc !== undefined && jc !== null) configContent += `Jc = ${jc}\n`;
    if (jmin !== undefined && jmin !== null) configContent += `Jmin = ${jmin}\n`;
    if (jmax !== undefined && jmax !== null) configContent += `Jmax = ${jmax}\n`;

    let peerCount = 0;
    if (Array.isArray(peers)) {
      for (const peer of peers) {
        if (!peer) continue;
        const cleanPublicKey = sanitizeConfigInput(peer.publicKey);
        if (!cleanPublicKey) continue;
        const cleanAllowedIps = sanitizeConfigInput(peer.allowedIps || peer.allowed_ips);
        const cleanEndpoint = sanitizeConfigInput(peer.endpoint);
        const keepalive = peer.persistentKeepalive || peer.persistent_keepalive;

        configContent += '\n[Peer]\n';
        configContent += `PublicKey = ${cleanPublicKey}\n`;
        if (cleanAllowedIps) configContent += `AllowedIPs = ${cleanAllowedIps}\n`;
        if (cleanEndpoint) configContent += `Endpoint = ${cleanEndpoint}\n`;
        if (keepalive) configContent += `PersistentKeepalive = ${keepalive}\n`;
        peerCount++;
      }
    }

    await fs.mkdir(path.dirname(meshConfigPath), { recursive: true });
    await fs.writeFile(meshConfigPath, configContent, 'utf-8');

    if (process.env.NODE_ENV !== 'test') {
      let interfaceExists = false;
      try {
        await execAsync(`ip link show ${iface}`);
        interfaceExists = true;
      } catch {
        interfaceExists = false;
      }

      if (interfaceExists) {
        try {
          await execAsync(`systemctl reload ${unitName}`);
        } catch (err: any) {
          const reloadErr = (err.stderr || err.stdout || err.message || 'systemctl reload error').trim();
          logger.warn({ err: reloadErr }, `Failed systemctl reload ${unitName}, attempting restart`);
          try {
            await execAsync(`systemctl restart ${unitName}`);
          } catch (restartErr: any) {
            const restartMsg = (restartErr.stderr || restartErr.stdout || restartErr.message || 'systemctl restart error').trim();
            logger.error({ err: restartMsg }, `Failed to restart mesh AWG service ${unitName}`);
            return callback(null, {
              success: false,
              message: `Failed to reload or restart mesh tunnel service (${unitName}): ${restartMsg}`
            });
          }
        }
      } else {
        try {
          await execAsync(`systemctl start ${unitName}`);
        } catch (startErr: any) {
          const startMsg = (startErr.stderr || startErr.stdout || startErr.message || 'systemctl start error').trim();
          logger.error({ err: startMsg }, `Failed to start mesh AWG service ${unitName}`);
          return callback(null, {
            success: false,
            message: `Failed to start mesh tunnel service (${unitName}): ${startMsg}`
          });
        }
      }

      try {
        await execAsync(`systemctl enable ${unitName}`);
      } catch (enableErr: any) {
        logger.warn({ err: enableErr.message }, `Failed to enable mesh AWG service ${unitName} for auto-start`);
      }
    }

    return callback(null, {
      success: true,
      message: `Mesh AWG tunnel interface ${iface} configured with ${peerCount} peer(s)`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to configure mesh AWG tunnel');
    return callback(null, { success: false, message: `Mesh tunnel configuration error: ${msg}` });
  }
}
