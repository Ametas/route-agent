import { ServerUnaryCall, sendUnaryData } from '@grpc/grpc-js';
import * as fs from 'fs/promises';
import path from 'path';
import http from 'http';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync } from '../utils/exec.js';
import { verifySecret, extractSecretFromMetadata, authenticateCall } from '../middleware/auth.js';
import { validateSafeCamouflagePath, fixXraySocketPermissions, resolveCaddyBinary } from '../utils/caddy.js';
import { getCaddyCertPaths } from '../utils/certStorage.js';
import { validateSingBoxConfig, atomicApplyAndReload, fixCaddyPermissions } from '../utils/singbox.js';
import { syncEgressFirewall, isUfwInstalled } from '../utils/firewall.js';
import { getAwgInterfaceName } from '../utils/awg.js';

const logger = pino({ level: 'info' });

function sanitizeConfigInput(val: string | number | undefined | null): string {
  if (val === undefined || val === null) return '';
  return String(val).replace(/[\r\n]/g, '').trim();
}

function getJson(url: string, timeoutMs = 3000): Promise<any> {
  return new Promise((resolve, reject) => {
    const req = http.get(url, (res) => {
      if (res.statusCode && (res.statusCode < 200 || res.statusCode >= 300)) {
        reject(new Error(`Status Code: ${res.statusCode}`));
        return;
      }
      let data = '';
      res.on('data', (chunk) => {
        data += chunk;
      });
      res.on('end', () => {
        try {
          resolve(JSON.parse(data));
        } catch (err) {
          reject(err);
        }
      });
    });

    req.on('error', (err) => {
      reject(err);
    });

    req.setTimeout(timeoutMs, () => {
      req.destroy();
      reject(new Error('Timeout'));
    });
  });
}

/**
 * RPC Обработчик метода ApplyConfig
 */
export async function applyConfigHandler(
  call: ServerUnaryCall<any, any>, 
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);

  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized gRPC execution blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  try {
    const rawConfig = call.request.configJson || call.request.config_json;
    const configObj = JSON.parse(rawConfig);
    const syntaxCheck = await validateSingBoxConfig(configObj);
    
    if (!syntaxCheck.valid) {
      return callback(null, { 
        success: false, 
        message: `Rejected by Node Agent: Invalid sing-box syntax. Error: ${syntaxCheck.error}` 
      });
    }

    await syncEgressFirewall(configObj);
    await atomicApplyAndReload(configObj);
    return callback(null, {
      success: true,
      message: 'Configuration successfully validated, applied, and sing-box reloaded via gRPC channel.'
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to process ApplyConfig RPC pipeline');
    return callback(null, { success: false, message: `Internal Agent Error: ${msg}` });
  }
}

/**
 * RPC Обработчик ConfigureCaddy (применение готового Caddyfile и распаковка заглушки)
 */
export async function configureCaddyHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized ConfigureCaddy request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const {
    caddyfileContent,
    caddyfile_content,
    camouflagePath,
    camouflage_path,
    camouflageHtml,
    camouflage_html,
    htmlContent,
    cloudflareApiToken,
    cloudflare_api_token,
    extraCertPem,
    extra_cert_pem,
    extraKeyPem,
    extra_key_pem,
    extraCertDomain,
    extra_cert_domain,
  } = call.request;

  try {
    const rawCaddyfile = caddyfileContent || caddyfile_content;
    const finalCaddyfile = typeof rawCaddyfile === 'string' ? rawCaddyfile : '';

    const rawCamouflagePath = camouflagePath || camouflage_path || '/var/www/camouflage';
    const sanitizedCamouflagePath = sanitizeConfigInput(rawCamouflagePath) || '/var/www/camouflage';
    const finalCamouflagePath = validateSafeCamouflagePath(sanitizedCamouflagePath);
    const finalCamouflageHtml = camouflageHtml || camouflage_html || htmlContent;

    if (finalCamouflageHtml && typeof finalCamouflageHtml === 'string' && finalCamouflageHtml.trim().length > 0) {
      await fs.mkdir(finalCamouflagePath, { recursive: true });
      await fs.writeFile(path.join(finalCamouflagePath, 'index.html'), finalCamouflageHtml, 'utf-8');
    }

    // Xeon-ring nodes only — egress nodes never send this field. Written to the EnvironmentFile
    // the Caddy systemd override references (see ensureCaddyCustomBinaryOverride), never inlined
    // into caddyfileContent itself. Must land BEFORE the Caddyfile write below: Caddy resolves
    // `{env.CF_API_TOKEN}` placeholders at config-load/validate time too, not just at runtime.
    const finalCloudflareToken = cloudflareApiToken || cloudflare_api_token;
    if (finalCloudflareToken && typeof finalCloudflareToken === 'string' && finalCloudflareToken.trim().length > 0) {
      const cfEnvPath = '/etc/caddy/cloudflare.env';
      await fs.mkdir(path.dirname(cfEnvPath), { recursive: true });
      await fs.writeFile(cfEnvPath, `CF_API_TOKEN=${finalCloudflareToken}\n`, { encoding: 'utf-8', mode: 0o600 });
      await fs.chmod(cfEnvPath, 0o600).catch(() => {});
    }

    // Egress nodes only — a wildcard cert/key relayed from a Xeon front by the orchestrator
    // (courier flow, see GetManagedCertificate), for static (non-ACME) use in a ring-relayed
    // site block. Written into the SAME on-disk layout Caddy's own ACME storage uses (see
    // certStorage.ts) so EgressSingBoxGenerator's existing certificate_path/key_path
    // construction on the orchestrator side needs no special-casing for this domain.
    const finalExtraCertPem = extraCertPem || extra_cert_pem;
    const finalExtraKeyPem = extraKeyPem || extra_key_pem;
    const finalExtraCertDomain = extraCertDomain || extra_cert_domain;
    if (
      finalExtraCertPem && typeof finalExtraCertPem === 'string' && finalExtraCertPem.trim().length > 0 &&
      finalExtraKeyPem && typeof finalExtraKeyPem === 'string' && finalExtraKeyPem.trim().length > 0 &&
      finalExtraCertDomain && typeof finalExtraCertDomain === 'string' && finalExtraCertDomain.trim().length > 0
    ) {
      const { certPath, keyPath } = getCaddyCertPaths(finalExtraCertDomain);
      await fs.mkdir(path.dirname(certPath), { recursive: true });
      await fs.writeFile(certPath, finalExtraCertPem, 'utf-8');
      await fs.writeFile(keyPath, finalExtraKeyPem, { encoding: 'utf-8', mode: 0o600 });
      await fs.chmod(keyPath, 0o600).catch(() => {});
      logger.info({ domain: finalExtraCertDomain, certPath }, 'Wrote relayed wildcard cert/key for ring-relayed site block');
    }

    const caddyfilePath = config.CADDYFILE_PATH || '/etc/caddy/Caddyfile';
    const caddyBackupPath = `${caddyfilePath}.bak`;
    const caddyDir = path.dirname(caddyfilePath);
    await fs.mkdir(caddyDir, { recursive: true });

    const caddyfileExists = await fs.stat(caddyfilePath).then(() => true).catch(() => false);
    if (caddyfileExists) {
      await fs.copyFile(caddyfilePath, caddyBackupPath).catch(() => {});
    }

    await fs.writeFile(caddyfilePath, finalCaddyfile, 'utf-8');

    await fixCaddyPermissions();
    await fixXraySocketPermissions();

    if (process.env.NODE_ENV !== 'test') {
      // 1. Предварительная валидация конфига самим бинарником Caddy
      // Резолвится по факту наличия файла на диске (resolveCaddyBinary), не по env-переменной —
      // агент читает свой env один раз при старте, а кастомный бинарник появляется уже во время
      // жизни процесса (uploadCaddyBinaryHandler), так что env-based резолюция никогда бы не
      // сработала без рестарта самого агента. На обычных egress-нодах кастомного бинарника нет —
      // поведение не меняется (bare `caddy` с $PATH, как и раньше).
      const caddyBin = await resolveCaddyBinary();
      try {
        await execAsync(`${caddyBin} validate --config "${caddyfilePath}"`);
      } catch (valErr: any) {
        if (caddyfileExists) {
          await fs.copyFile(caddyBackupPath, caddyfilePath).catch(() => {});
        }
        const valMsg = (valErr.stderr || valErr.stdout || valErr.message || 'Ошибка синтаксиса Caddyfile').trim();
        logger.error({ err: valMsg }, 'Caddyfile validation failed');
        return callback(null, {
          success: false,
          message: `Ошибка синтаксиса Caddyfile:\n${valMsg}`
        });
      }

      // 2. Мягкая перезагрузка службы после успешной валидации
      try {
        const reloadCmd = config.CADDY_RELOAD_COMMAND || 'systemctl reload caddy || systemctl restart caddy';
        await execAsync(reloadCmd);
      } catch (err: unknown) {
        if (caddyfileExists) {
          await fs.copyFile(caddyBackupPath, caddyfilePath).catch(() => {});
        }
        
        let journalLogs = '';
        try {
          const { stdout } = await execAsync('journalctl -u caddy -n 12 --no-pager');
          journalLogs = stdout.trim();
        } catch {}

        const msg = err instanceof Error ? err.message : String(err);
        logger.warn({ err: msg, journalLogs }, 'Failed to reload Caddy service');
        
        return callback(null, {
          success: false,
          message: `Сбой службы Caddy при reload:\n${journalLogs || msg}`
        });
      }
    }

    return callback(null, {
      success: true,
      message: 'Caddyfile успешно валидирован и применен.'
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg }, 'Failed to configure Caddy');
    return callback(null, { success: false, message: msg });
  }
}

/**
 * RPC Обработчик GetManagedCertificate — читает PEM-содержимое сертификата, реально выпущенного
 * и хранимого локальным Caddy (certmagic on-disk storage), для указанного домена. Используется
 * оркестратором как courier-шаг: забрать wildcard-сертификат, выпущенный кастомным Caddy на
 * Xeon-ring ноде через DNS-01, и переслать его на egress-ноды (см. ConfigureCaddy's
 * extra_cert_pem/extra_key_pem). Только чтение — не выпускает и не изменяет сертификаты.
 */
export async function getManagedCertificateHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  if (!authenticateCall(call)) {
    logger.warn('Unauthorized GetManagedCertificate request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { domain } = call.request;
  if (!domain || typeof domain !== 'string' || domain.trim().length === 0) {
    return callback(null, { success: false, message: 'Missing domain parameter.' });
  }

  try {
    const { certPath, keyPath } = getCaddyCertPaths(domain);
    const [fullchainPem, privateKeyPem] = await Promise.all([
      fs.readFile(certPath, 'utf-8'),
      fs.readFile(keyPath, 'utf-8'),
    ]);

    return callback(null, {
      success: true,
      message: 'OK',
      fullchainPem,
      privateKeyPem,
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.warn({ err: msg, domain }, 'Managed certificate not found or unreadable (issuance may still be in progress)');
    return callback(null, { success: false, message: msg });
  }
}

/**
 * RPC Обработчик ConfigureOlcrtc (настройка и управление службой olcrtc-manager)
 */
export async function configureOlcrtcHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized ConfigureOlcrtc request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const { enabled, user, password, port } = call.request;

  try {
    const servicePort = port || 8888;
    const managerBin = config.OLCRTC_MANAGER_BINARY_PATH || '/usr/local/bin/olcrtc-manager';

    if (!enabled) {
      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync('systemctl stop olcrtc || true');
          await execAsync('systemctl disable olcrtc || true');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to stop/disable olcrtc service');
        }
      }
      return callback(null, {
        success: true,
        message: 'olcrtc service successfully disabled and stopped.'
      });
    }

    if (process.env.NODE_ENV !== 'test') {
      const managerExists = await fs.stat(managerBin).then(() => true).catch(() => false);
      if (!managerExists) {
        logger.warn({ path: managerBin }, 'olcrtc-manager binary is missing when configuring service');
      }
    }

    const serviceContent = `[Unit]
Description=OpenLibreCommunity WebRTC Manager Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=${managerBin} --port ${servicePort}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`;

    const servicePath = '/etc/systemd/system/olcrtc.service';
    await fs.mkdir(path.dirname(servicePath), { recursive: true }).catch(() => {});
    await fs.writeFile(servicePath, serviceContent, 'utf-8');

    if (process.env.NODE_ENV !== 'test') {
      await execAsync('systemctl daemon-reload');
      await execAsync('systemctl enable --now olcrtc');

      // Открываем порт в UFW
      if (await isUfwInstalled()) {
        try {
          await execAsync(`sudo ufw allow ${servicePort}`);
          await execAsync('sudo ufw reload');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to configure UFW port for olcrtc service');
        }
      }

      if (user && password) {
        const authMeUrl = `http://127.0.0.1:${servicePort}/api/auth/me`;
        const setupUrl = `http://127.0.0.1:${servicePort}/api/auth/setup`;

        for (let i = 0; i < 10; i++) {
          try {
            await getJson(authMeUrl, 1000);
            break;
          } catch {
            await new Promise((res) => setTimeout(res, 500));
          }
        }

        try {
          await new Promise<void>((resolve, reject) => {
            const reqData = JSON.stringify({ user, password });
            const req = http.request(setupUrl, {
              method: 'POST',
              headers: {
                'Content-Type': 'application/json',
                'Content-Length': Buffer.byteLength(reqData)
              }
            }, (res) => {
              res.resume();
              resolve();
            });
            req.on('error', reject);
            req.write(reqData);
            req.end();
          });
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to POST auth setup to olcrtc-manager');
        }
      }
    }

    return callback(null, {
      success: true,
      message: `olcrtc service configured and enabled on port ${servicePort}.`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to configure Olcrtc service');
    return callback(null, { success: false, message: `Olcrtc configuration error: ${msg}` });
  }
}

/**
 * RPC Обработчик ConfigureAwg (дистанционная настройка и управление L3-интерфейсом AmneziaWG 3.0)
 */
export async function configureAwgHandler(
  call: ServerUnaryCall<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  const secretHeader = extractSecretFromMetadata(call);
  if (!verifySecret(secretHeader)) {
    logger.warn('Unauthorized ConfigureAwg request blocked');
    return callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
  }

  const {
    enabled,
    port,
    serverPrivateKey,
    serverPublicKey,
    addressV4,
    addressV6,
    jc,
    jmin,
    jmax,
    s1,
    s2,
    s3,
    s4,
    h1,
    h2,
    h3,
    h4,
    i1,
    i2,
    i3,
    i4,
    i5,
    headerProtectionKey,
    peers,
    ipv6Mode
  } = call.request;

  try {
    const awgConfigPath = config.AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/awg0.conf';
    const iface = getAwgInterfaceName();
    const unitName = `route-awg@${iface}`;

    if (!enabled) {
      if (process.env.NODE_ENV !== 'test') {
        try {
          await execAsync(`systemctl stop ${unitName} || systemctl stop awg-quick@${iface} || awg-quick down ${iface} || true`);
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Error stopping AmneziaWG interface service');
        }

        if (port && (await isUfwInstalled())) {
          try {
            await execAsync(`sudo ufw delete allow ${port}/udp || true`);
            await execAsync('sudo ufw reload || true');
          } catch (err: any) {
            logger.warn({ err: err.message }, 'Error closing UFW port for AmneziaWG');
          }
        }
      }

      return callback(null, {
        success: true,
        message: 'AmneziaWG (AWG3) service successfully disabled and stopped.'
      });
    }

    // 1. Системные настройки (Sysctl)
    if (process.env.NODE_ENV !== 'test') {
      try {
        await execAsync('sysctl -w net.ipv4.ip_forward=1');
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to set sysctl net.ipv4.ip_forward=1');
      }

      if (ipv6Mode === 'dual-stack' || ipv6Mode === 'trap-ipv6') {
        try {
          await execAsync('sysctl -w net.ipv6.conf.all.forwarding=1');
        } catch (err: any) {
          logger.debug({ err: err.message }, 'IPv6 disabled in kernel; skipping net.ipv6.conf.all.forwarding sysctl setting');
        }
      }
    }

    // 2. Генерация конфигурации /etc/amnezia/amneziawg/awg0.conf
    const cleanServerPrivateKey = sanitizeConfigInput(serverPrivateKey);
    const cleanServerPublicKey = sanitizeConfigInput(serverPublicKey);
    const cleanAddressV4 = sanitizeConfigInput(addressV4);
    const cleanAddressV6 = sanitizeConfigInput(addressV6);
    const cleanHeaderProtectionKey = sanitizeConfigInput(headerProtectionKey);
    const cleanI1 = sanitizeConfigInput(i1);
    const cleanI2 = sanitizeConfigInput(i2);
    const cleanI3 = sanitizeConfigInput(i3);
    const cleanI4 = sanitizeConfigInput(i4);
    const cleanI5 = sanitizeConfigInput(i5);

    const addresses: string[] = [];
    if (cleanAddressV4) addresses.push(cleanAddressV4);
    if (cleanAddressV6 && ipv6Mode !== 'ipv4-only') addresses.push(cleanAddressV6);

    let configContent = '[Interface]\n';
    if (cleanServerPrivateKey) configContent += `PrivateKey = ${cleanServerPrivateKey}\n`;
    if (port) configContent += `ListenPort = ${port}\n`;
    if (addresses.length > 0) configContent += `Address = ${addresses.join(', ')}\n`;

    if (jc !== undefined && jc !== null) configContent += `Jc = ${jc}\n`;
    if (jmin !== undefined && jmin !== null) configContent += `Jmin = ${jmin}\n`;
    if (jmax !== undefined && jmax !== null) configContent += `Jmax = ${jmax}\n`;
    if (s1 !== undefined && s1 !== null && s1 !== '') configContent += `S1 = ${sanitizeConfigInput(s1)}\n`;
    if (s2 !== undefined && s2 !== null && s2 !== '') configContent += `S2 = ${sanitizeConfigInput(s2)}\n`;
    if (s3 !== undefined && s3 !== null && s3 !== '') configContent += `S3 = ${sanitizeConfigInput(s3)}\n`;
    if (s4 !== undefined && s4 !== null && s4 !== '') configContent += `S4 = ${sanitizeConfigInput(s4)}\n`;
    if (h1 !== undefined && h1 !== null && h1 !== '') configContent += `H1 = ${sanitizeConfigInput(h1)}\n`;
    if (h2 !== undefined && h2 !== null && h2 !== '') configContent += `H2 = ${sanitizeConfigInput(h2)}\n`;
    if (h3 !== undefined && h3 !== null && h3 !== '') configContent += `H3 = ${sanitizeConfigInput(h3)}\n`;
    if (h4 !== undefined && h4 !== null && h4 !== '') configContent += `H4 = ${sanitizeConfigInput(h4)}\n`;
    if (cleanI1) configContent += `I1 = ${cleanI1}\n`;
    if (cleanI2) configContent += `I2 = ${cleanI2}\n`;
    if (cleanI3) configContent += `I3 = ${cleanI3}\n`;
    if (cleanI4) configContent += `I4 = ${cleanI4}\n`;
    if (cleanI5) configContent += `I5 = ${cleanI5}\n`;
    if (cleanHeaderProtectionKey) configContent += `HeaderProtectionKey = ${cleanHeaderProtectionKey}\n`;

    if (Array.isArray(peers)) {
      for (const peer of peers) {
        if (!peer) continue;
        const cleanPublicKey = sanitizeConfigInput(peer.publicKey);
        if (!cleanPublicKey) continue;
        const cleanPresharedKey = sanitizeConfigInput(peer.presharedKey);
        const cleanAllowedIps = sanitizeConfigInput(peer.allowedIps);

        configContent += '\n[Peer]\n';
        configContent += `PublicKey = ${cleanPublicKey}\n`;
        if (cleanPresharedKey) configContent += `PresharedKey = ${cleanPresharedKey}\n`;
        if (cleanAllowedIps) configContent += `AllowedIPs = ${cleanAllowedIps}\n`;
      }
    }

    await fs.mkdir(path.dirname(awgConfigPath), { recursive: true });
    await fs.writeFile(awgConfigPath, configContent, 'utf-8');

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
          // Горячая перезагрузка конфига через systemctl reload route-awg@awg0 (исполняет ExecReload в юните)
          await execAsync(`systemctl reload ${unitName}`);
        } catch (err: any) {
          const reloadErr = (err.stderr || err.stdout || err.message || 'systemctl reload error').trim();
          logger.warn({ err: reloadErr }, `Failed systemctl reload ${unitName}, attempting restart`);
          try {
            await execAsync(`systemctl restart ${unitName}`);
          } catch (restartErr: any) {
            const restartMsg = (restartErr.stderr || restartErr.stdout || restartErr.message || 'systemctl restart error').trim();
            logger.error({ err: restartMsg }, `Failed to restart AWG service ${unitName}`);
            return callback(null, {
              success: false,
              message: `Failed to reload or restart AWG service (${unitName}): ${restartMsg}`
            });
          }
        }
      } else {
        try {
          await execAsync(`systemctl start ${unitName}`);
        } catch (startErr: any) {
          const startMsg = (startErr.stderr || startErr.stdout || startErr.message || 'systemctl start error').trim();
          logger.error({ err: startMsg }, `Failed to start AWG service ${unitName}`);
          return callback(null, {
            success: false,
            message: `Failed to start AWG service (${unitName}): ${startMsg}`
          });
        }
      }

      try {
        await execAsync(`systemctl enable ${unitName}`);
      } catch (enableErr: any) {
        logger.warn({ err: enableErr.message }, `Failed to enable AWG service ${unitName} for auto-start`);
      }

      try {
        await execAsync('iptables -C FORWARD -i awg0 -j ACCEPT || iptables -A FORWARD -i awg0 -j ACCEPT');

        try {
          const { stdout } = await execAsync('ip route show default');
          const match = stdout.match(/dev\s+([^\s]+)/);
          const defaultIface = match ? match[1] : null;
          if (defaultIface) {
            await execAsync(`iptables -t nat -C POSTROUTING -o ${defaultIface} -j MASQUERADE || iptables -t nat -A POSTROUTING -o ${defaultIface} -j MASQUERADE`);
          }
        } catch {}
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to configure iptables NAT rules for awg0');
      }

      if (ipv6Mode === 'trap-ipv6') {
        try {
          await execAsync('ip6tables -C FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited || ip6tables -A FORWARD -i awg0 -j REJECT --reject-with icmp6-adm-prohibited');
        } catch (err: any) {
          logger.debug({ err: err.message }, 'IPv6/ip6tables disabled in kernel or not available; skipping trap-ipv6 rules');
        }
      }

      if (port && (await isUfwInstalled())) {
        try {
          await execAsync(`sudo ufw allow ${port}/udp`);
          await execAsync('sudo ufw reload');
        } catch (err: any) {
          logger.warn({ err: err.message }, 'Failed to configure UFW port for AmneziaWG');
        }
      }
    }

    return callback(null, {
      success: true,
      message: `AmneziaWG (AWG3) interface awg0 configured and active on port ${port || 51820}.`
    });
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : 'Unknown error';
    logger.error({ err: msg }, 'Failed to configure AmneziaWG service');
    return callback(null, { success: false, message: `AmneziaWG configuration error: ${msg}` });
  }
}
