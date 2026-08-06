// tests/app.test.ts

import test from 'node:test';
import assert from 'node:assert';
import { promises as fs } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { computeProtoContractHash, clearProtoContractHashCache, getCanonicalSchemaFromPackageDef } from '../src/services/protoContract.service.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const tempDir = path.join(__dirname, 'temp');
const tempConfigPath = path.join(tempDir, 'sing-box-config.json');
const tempBinaryPath = path.join(tempDir, 'sing-box');
const tempCaddyfilePath = path.join(tempDir, 'Caddyfile');
const tempOlcrtcPath = path.join(tempDir, 'olcrtc');
const tempOlcrtcManagerPath = path.join(tempDir, 'olcrtc-manager');
const tempAwgPath = path.join(tempDir, 'awg0.conf');

const tempAwgToolsBinaryPath = path.join(tempDir, 'awg');
const tempAwgQuickBinaryPath = path.join(tempDir, 'awg-quick');
const tempAwgGoBinaryPath = path.join(tempDir, 'amneziawg-go');
const tempAwgUnitPath = path.join(tempDir, 'route-awg@.service');
const tempSingboxUnitPath = path.join(tempDir, 'sing-box.service');

// Фикстурные mTLS-сертификаты для тестов. С тех пор, как агент отказывается стартовать
// без валидных сертификатов (см. security-аудит), даже "обычный" пайплайн-тест обязан
// поднимать сервер в mTLS-режиме — insecure-режима у агента больше не существует.
const fixtureCertsDir = path.join(__dirname, 'fixtures', 'certs');

// Конфигурируем тестовое окружение до загрузки модулей
process.env.NODE_ENV = 'test';
process.env.PORT = '8082';
process.env.HOST = '127.0.0.1';
process.env.EGRESS_CONTROL_SECRET = 'test-secret-123';
process.env.CA_CERT_PATH = path.join(fixtureCertsDir, 'ca.crt');
process.env.AGENT_CERT_PATH = path.join(fixtureCertsDir, 'agent.crt');
process.env.AGENT_KEY_PATH = path.join(fixtureCertsDir, 'agent.key');
process.env.SINGBOX_CONFIG_PATH = tempConfigPath;
process.env.SINGBOX_BINARY_PATH = tempBinaryPath;
process.env.CADDYFILE_PATH = tempCaddyfilePath;
process.env.OLCRTC_BINARY_PATH = tempOlcrtcPath;
process.env.OLCRTC_MANAGER_BINARY_PATH = tempOlcrtcManagerPath;
process.env.AWG_CONFIG_PATH = tempAwgPath;
process.env.AWG_TOOLS_BINARY_PATH = tempAwgToolsBinaryPath;
process.env.AWG_QUICK_BINARY_PATH = tempAwgQuickBinaryPath;
process.env.AWG_GO_BINARY_PATH = tempAwgGoBinaryPath;
process.env.AWG_UNIT_FILE_PATH = tempAwgUnitPath;
process.env.SINGBOX_UNIT_FILE_PATH = tempSingboxUnitPath;
process.env.RELOAD_COMMAND = 'echo "mock reload"';
process.env.CADDY_RELOAD_COMMAND = 'echo "mock caddy reload"';

// Импортируем наш скомпилированный gRPC сервер для инициализации биндинга
const { startServer, getGrpcServerCredentials } = await import('../src/index.js');

const PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');
const packageDefinition = protoLoader.loadSync(PROTO_PATH, { keepCase: false });
const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
const EgressAgentService = protoDescriptor.agent.EgressAgentService;

test('Route Agent gRPC Pipeline Testing', async (t) => {
  await fs.rm(tempDir, { recursive: true, force: true }).catch(() => {});
  await fs.mkdir(tempDir, { recursive: true });

  const server = await startServer();

  // Создаем нативный mTLS-клиент для тестов (сервер больше не поднимает insecure-режим)
  const caCert = await fs.readFile(path.join(fixtureCertsDir, 'ca.crt'));
  const clientCert = await fs.readFile(path.join(fixtureCertsDir, 'client.crt'));
  const clientKey = await fs.readFile(path.join(fixtureCertsDir, 'client.key'));
  const client = new EgressAgentService(
    '127.0.0.1:8082',
    grpc.credentials.createSsl(caCert, clientKey, clientCert)
  );

  const validMetadata = new grpc.Metadata();
  validMetadata.add('x-orchestrator-secret', 'test-secret-123');

  t.after(async () => {
    client.close();
    server.forceShutdown();
    await fs.rm(tempDir, { recursive: true, force: true }).catch(() => {});
  });

  await t.test('ApplyConfig should block requests with invalid metadata tokens', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'malicious_token');

    client.applyConfig({ configJson: '{}' }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('ApplyConfig should return success if sing-box configuration syntax is valid', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const payload = { log: { level: 'info' } };

    client.applyConfig({ config_json: JSON.stringify(payload), configJson: JSON.stringify(payload) }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      done();
    });
  });

  await t.test('StreamTelemetry should stream telemetry containing webrtcStatus and singboxVersion', (t, done) => {
    const stream = client.streamTelemetry({ orchestrator_secret: 'test-secret-123', orchestratorSecret: 'test-secret-123' });
    
    stream.on('data', (data: any) => {
      try {
        const webrtcStat = data.webrtc_status || data.webrtcStatus;
        const sbVer = data.singbox_version || data.singboxVersion;
        const awgPeers = data.awg_active_peers ?? data.awgActivePeers;
        const awgVer = data.awg_version || data.awgVersion;
        const awgStat = data.awg_status || data.awgStatus;
        const awgToolsVer = data.awg_tools_version || data.awgToolsVersion;
        const awgGoVer = data.awg_go_version || data.awgGoVersion;

        assert.ok(webrtcStat !== undefined, 'webrtc_status must be present');
        assert.ok(sbVer !== undefined, 'singbox_version must be present');
        assert.ok(awgPeers !== undefined, 'awg_active_peers must be present');
        assert.ok(awgVer !== undefined, 'awg_version must be present');
        assert.ok(awgStat !== undefined, 'awg_status must be present');
        assert.ok(awgToolsVer !== undefined, 'awg_tools_version must be present');
        assert.ok(awgGoVer !== undefined, 'awg_go_version must be present');

        assert.strictEqual(webrtcStat, 'nominal');
        assert.strictEqual(typeof sbVer, 'string');
        assert.strictEqual(typeof awgPeers, 'number');
        assert.strictEqual(typeof awgVer, 'string');
        assert.strictEqual(typeof awgStat, 'string');
        assert.strictEqual(typeof awgToolsVer, 'string');
        assert.strictEqual(typeof awgGoVer, 'string');
        assert.strictEqual(typeof (data.cpu_usage ?? data.cpuUsage), 'number');
        assert.strictEqual(typeof (data.mem_usage ?? data.memUsage), 'number');
        assert.strictEqual(typeof (data.active_connections ?? data.activeConnections), 'number');
        assert.strictEqual(typeof (data.system_logs ?? data.systemLogs), 'string');
        stream.destroy();
        done();
      } catch (err) {
        stream.destroy();
        done(err);
      }
    });

    stream.on('error', (err: any) => {
      // Ignored if stream is already destroyed
    });
  });

  await t.test('GetTelemetry should return instant telemetry frame via Unary RPC', (t, done) => {
    client.getTelemetry({ orchestrator_secret: 'test-secret-123', orchestratorSecret: 'test-secret-123' }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      const webrtcStat = response.webrtc_status || response.webrtcStatus;
      const sbVer = response.singbox_version || response.singboxVersion;
      const awgPeers = response.awg_active_peers ?? response.awgActivePeers;
      const awgVer = response.awg_version || response.awgVersion;
      const awgStat = response.awg_status || response.awgStatus;
      const awgToolsVer = response.awg_tools_version || response.awgToolsVersion;
      const awgGoVer = response.awg_go_version || response.awgGoVersion;

      assert.ok(webrtcStat !== undefined, 'webrtc_status must be present');
      assert.ok(sbVer !== undefined, 'singbox_version must be present');
      assert.ok(awgPeers !== undefined, 'awg_active_peers must be present');
      assert.ok(awgVer !== undefined, 'awg_version must be present');
      assert.ok(awgStat !== undefined, 'awg_status must be present');
      assert.ok(awgToolsVer !== undefined, 'awg_tools_version must be present');
      assert.ok(awgGoVer !== undefined, 'awg_go_version must be present');

      assert.strictEqual(typeof sbVer, 'string');
      assert.strictEqual(typeof awgPeers, 'number');
      assert.strictEqual(typeof awgVer, 'string');
      assert.strictEqual(typeof awgStat, 'string');
      assert.strictEqual(typeof awgToolsVer, 'string');
      assert.strictEqual(typeof awgGoVer, 'string');
      assert.strictEqual(typeof (response.cpu_usage ?? response.cpuUsage), 'number');
      assert.strictEqual(typeof response.memUsage, 'number');
      assert.strictEqual(typeof response.activeConnections, 'number');
      assert.strictEqual(typeof response.systemLogs, 'string');
      done();
    });
  });

  await t.test('UploadSingboxBinary should block stream with invalid secret', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'invalid_secret');

    let doneCalled = false;
    const finish = (e?: any) => {
      if (!doneCalled) {
        doneCalled = true;
        done(e);
      }
    };

    const call = client.uploadSingboxBinary(badMetadata, (err: any, response: any) => {
      if (err) {
        assert.ok(err);
        return finish();
      }
      assert.strictEqual(response.success, false);
      finish();
    });

    call.on('error', () => {
      finish();
    });

    call.write({ orchestratorSecret: 'invalid_secret', chunk: Buffer.from('test data'), version: '1.12.0', isFinal: true });
    call.end();
  });

  await t.test('UploadSingboxBinary should upload binary chunks and update target binary file', (t, done) => {
    const call = client.uploadSingboxBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('1.12.0'));
        const exists = await fs.stat(tempBinaryPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, true);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('binary_chunk_1\n'), version: '1.12.0', isFinal: false });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('binary_chunk_2\n'), version: '1.12.0', isFinal: true });
    call.end();
  });

  await t.test('UploadOlcrtcBinary should upload olcrtc / olcrtc-manager binaries', (t, done) => {
    const call = client.uploadOlcrtcBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('olcrtc-manager'));
        const exists = await fs.stat(tempOlcrtcManagerPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, true);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('olcrtc_mgr_chunk'), version: '1.0.0', targetBinary: 'olcrtc-manager', isFinal: true });
    call.end();
  });

  await t.test('UploadOlcrtcBinary should sanitize path traversal targetBinary to olcrtc-manager fallback', (t, done) => {
    const call = client.uploadOlcrtcBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('path_traversal_test'), version: '1.0.0', targetBinary: '../../etc/malicious', isFinal: true });
    call.end();
  });

  await t.test('sanitizeAwgToolsSymlink health check should remove invalid symlink pointing to amneziawg-go', async () => {
    const { sanitizeAwgToolsSymlink } = await import('../src/services/binary.service.js');
    await fs.mkdir(path.dirname(tempAwgToolsBinaryPath), { recursive: true });
    await fs.unlink(tempAwgToolsBinaryPath).catch(() => {});

    try {
      // Create invalid symlink pointing to amneziawg-go
      await fs.symlink('amneziawg-go', tempAwgToolsBinaryPath);
      const isSymlinkBefore = await fs.lstat(tempAwgToolsBinaryPath).then(s => s.isSymbolicLink()).catch(() => false);
      assert.strictEqual(isSymlinkBefore, true);

      await sanitizeAwgToolsSymlink();

      const existsAfter = await fs.lstat(tempAwgToolsBinaryPath).then(() => true).catch(() => false);
      assert.strictEqual(existsAfter, false, 'invalid symlink must be removed by sanitizeAwgToolsSymlink');
    } catch (err: any) {
      if (err.code === 'EPERM' && process.platform === 'win32') {
        // On Windows without privilege, symlink creation requires admin rights.
        // Verify sanitizeAwgToolsSymlink handles non-symlink / missing files safely.
        await sanitizeAwgToolsSymlink();
      } else {
        throw err;
      }
    }
  });

  await t.test('ensureAwgSystemdUnit idempotency check', async () => {
    const { ensureAwgSystemdUnit } = await import('../src/services/systemdUnit.service.js');
    const customTestUnitPath = path.join(tempDir, 'test-route-awg@.service');
    await fs.unlink(customTestUnitPath).catch(() => {});

    // 1. Initial call on clean filesystem creates the file with expected content
    const firstCallResult = await ensureAwgSystemdUnit(customTestUnitPath);
    assert.strictEqual(firstCallResult, true, 'First call should create unit file');

    const content = await fs.readFile(customTestUnitPath, 'utf-8');
    assert.ok(content.includes('Description=AmneziaWG interface %i (managed by route-agent)'));
    assert.ok(content.includes('ExecStart='));
    assert.ok(content.includes('ExecStop='));
    assert.ok(content.includes('ExecReload='));
    assert.ok(content.includes(`${tempAwgToolsBinaryPath}-quick up %i`));
    assert.ok(content.includes(`${tempAwgToolsBinaryPath} syncconf %i <(${tempAwgToolsBinaryPath}-quick strip %i)`));

    // 2. Second call with unchanged content should return false without rewriting file or triggering daemon-reload
    const secondCallResult = await ensureAwgSystemdUnit(customTestUnitPath);
    assert.strictEqual(secondCallResult, false, 'Second call with identical content must return false');
  });

  await t.test('ensureSingboxSystemdUnit idempotency check (mirrors ensureAwgSystemdUnit)', async () => {
    const { ensureSingboxSystemdUnit } = await import('../src/utils/singbox.js');
    const customTestUnitPath = path.join(tempDir, 'test-sing-box.service');
    await fs.unlink(customTestUnitPath).catch(() => {});

    const execCalls: string[] = [];
    const spyExec = async (command: string) => {
      execCalls.push(command);
      return { stdout: '', stderr: '' };
    };

    // 1. Initial call on clean filesystem creates the file with expected content, unchanged
    // from what install.sh used to embed verbatim.
    const firstCallResult = await ensureSingboxSystemdUnit(customTestUnitPath, spyExec);
    assert.strictEqual(firstCallResult, true, 'First call should create unit file');

    const content = await fs.readFile(customTestUnitPath, 'utf-8');
    assert.ok(content.includes('Description=sing-box service'));
    assert.ok(content.includes('CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE'));
    assert.ok(content.includes('AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE'));
    assert.ok(content.includes(`ExecStart=${tempBinaryPath} run -c ${tempConfigPath}`));
    assert.ok(content.includes('Restart=always'));
    assert.ok(content.includes('RestartSec=5'));
    assert.ok(content.includes(`ExecReload=/bin/sh -c "${tempBinaryPath} check -c ${tempConfigPath} && /bin/kill -HUP $MAINPID"`));
    assert.ok(content.includes('WantedBy=multi-user.target'));

    // 2. Second call with unchanged content should return false without rewriting the file or
    // triggering daemon-reload. NODE_ENV=test already skips the daemon-reload exec internally,
    // but we assert no exec call happened at all for either call, since the write itself is
    // what would precede it, and no exec is expected either way at NODE_ENV=test.
    const secondCallResult = await ensureSingboxSystemdUnit(customTestUnitPath, spyExec);
    assert.strictEqual(secondCallResult, false, 'Second call with identical content must return false');
    assert.strictEqual(execCalls.length, 0, 'daemon-reload must not run under NODE_ENV=test');
  });

  await t.test('resolveSingboxReloadCommand should fall back to start when sing-box is not active', async () => {
    const { resolveSingboxReloadCommand } = await import('../src/utils/singbox.js');

    // Mocked systemctl is-active reporting the unit as active -> keep the configured reload.
    const activeExec = async (command: string) => {
      assert.strictEqual(command, 'systemctl is-active sing-box');
      return { stdout: 'active\n', stderr: '' };
    };
    const activeCmd = await resolveSingboxReloadCommand(activeExec);
    assert.strictEqual(activeCmd, process.env.RELOAD_COMMAND);

    // Mocked systemctl is-active reporting the unit as inactive (resolved, not rejected) ->
    // must fall back to start, not reload.
    const inactiveExec = async (_command: string) => ({ stdout: 'inactive\n', stderr: '' });
    const inactiveCmd = await resolveSingboxReloadCommand(inactiveExec);
    assert.strictEqual(inactiveCmd, 'systemctl start sing-box');

    // Real `systemctl is-active` rejects the promise (non-zero exit) for inactive/failed/unknown
    // units -- must be treated the same way as an explicit "inactive" result above.
    const rejectingExec = async (_command: string) => {
      const err: any = new Error('Command failed: systemctl is-active sing-box\ninactive\n');
      err.stdout = 'inactive\n';
      throw err;
    };
    const rejectingCmd = await resolveSingboxReloadCommand(rejectingExec);
    assert.strictEqual(rejectingCmd, 'systemctl start sing-box');
  });

  await t.test('getAwgInterfaceName and restartActiveAwgServices when unconfigured', async () => {
    const { getAwgInterfaceName, restartActiveAwgServices } = await import('../src/utils/awg.js');
    assert.strictEqual(getAwgInterfaceName(), 'awg0');

    const res = await restartActiveAwgServices();
    assert.strictEqual(res.reloaded, false);
    assert.strictEqual(res.restartedUnits.length, 0);
  });

  await t.test('checkAndHealAwgInterface self-healing and rate-limiting test', async () => {
    const { checkAndHealAwgInterface, getAwgHealthState } = await import('../src/utils/telemetry.js');

    // 1. When interface is configured and alive
    delete process.env.TEST_AWG_STATUS;
    process.env.TEST_AWG_UNCONFIGURED = 'false';
    process.env.TEST_AWG_ALIVE = 'true';

    const stateAlive = await checkAndHealAwgInterface('awg0');
    assert.strictEqual(stateAlive.status, 'nominal');

    // 2. When interface falls down (crashes), self-healing is triggered
    process.env.TEST_AWG_ALIVE = 'false';
    const stateCrashed = await checkAndHealAwgInterface('awg0');
    assert.strictEqual(stateCrashed.autoRestartCount, 1);
    assert.ok(stateCrashed.status.startsWith('crashed (restarted'));

    // 3. Immediate second check while still down is rate-limited (5 min cooldown)
    const stateRateLimited = await checkAndHealAwgInterface('awg0');
    assert.strictEqual(stateRateLimited.autoRestartCount, 1, 'Restart count should not increase due to 5m rate-limiting');

    // 4. When interface recovers and becomes alive again
    process.env.TEST_AWG_ALIVE = 'true';
    const stateRecovered = await checkAndHealAwgInterface('awg0');
    assert.ok(stateRecovered.status.startsWith('nominal (restarted'));

    // Cleanup env overrides
    delete process.env.TEST_AWG_UNCONFIGURED;
    delete process.env.TEST_AWG_ALIVE;
  });

  await t.test('UploadAwgToolsBinary should upload awg tools binary', (t, done) => {
    const call = client.uploadAwgToolsBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('1.0.20260618-2'));
        const exists = await fs.stat(tempAwgToolsBinaryPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, true);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('awg_tools_binary_data'), version: '1.0.20260618-2', isFinal: true });
    call.end();
  });

  await t.test('UploadAwgToolsBinary with target_binary === awg-quick should save executable script', (t, done) => {
    const call = client.uploadAwgToolsBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('awg-quick script'));
        const exists = await fs.stat(tempAwgQuickBinaryPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, true);

        const content = await fs.readFile(tempAwgQuickBinaryPath, 'utf-8');
        assert.ok(content.startsWith('#!/bin/sh'));
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({
      orchestratorSecret: 'test-secret-123',
      chunk: Buffer.from('#!/bin/sh\necho "awg-quick mock script"\n'),
      version: '1.0.20260618-2',
      targetBinary: 'awg-quick',
      isFinal: true
    });
    call.end();
  });

  await t.test('UploadAwgToolsBinary with target_binary === awg-quick should reject script without shebang', (t, done) => {
    const call = client.uploadAwgToolsBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, false);
        assert.ok(response.message.includes('missing shebang'));
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({
      orchestratorSecret: 'test-secret-123',
      chunk: Buffer.from('echo "no shebang header"\n'),
      version: '1.0.20260618-2',
      targetBinary: 'awg-quick',
      isFinal: true
    });
    call.end();
  });

  await t.test('UploadAwgGoBinary should upload amneziawg-go binary', (t, done) => {
    const call = client.uploadAwgGoBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('0.0.20230223'));
        const exists = await fs.stat(tempAwgGoBinaryPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, true);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('awg_go_binary_data'), version: '0.0.20230223', isFinal: true });
    call.end();
  });

  await t.test('UpgradeSingbox should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.upgradeSingbox({ version: '1.12.0', downloadUrl: 'http://example.com/binary' }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('UpgradeSingbox should reject missing download_url', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.upgradeSingbox({ version: '1.12.0', downloadUrl: '' }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Missing download_url in request payload.');
      done();
    });
  });

  await t.test('ConfigureCaddy should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.configureCaddy({ caddyfileContent: 'example.com {}' }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('ConfigureCaddy should apply caddyfileContent and unpack camouflage html when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const tempCamouflageDir = path.join(tempDir, 'camouflage');
    const payload = {
      caddyfileContent: 'example.com {\n  root * /var/www\n}',
      camouflageHtml: '<html><body>Camouflage Site</body></html>',
      camouflagePath: tempCamouflageDir
    };

    client.configureCaddy(payload, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.strictEqual(response.message, 'Caddyfile успешно валидирован и применен.');

        const htmlContent = await fs.readFile(path.join(tempCamouflageDir, 'index.html'), 'utf-8');
        assert.strictEqual(htmlContent, '<html><body>Camouflage Site</body></html>');

        const caddyContent = await fs.readFile(tempCaddyfilePath, 'utf-8');
        assert.strictEqual(caddyContent, 'example.com {\n  root * /var/www\n}');
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('ConfigureOlcrtc should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.configureOlcrtc({ enabled: true, user: 'admin', password: 'pass', port: 8888 }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('ConfigureOlcrtc should configure Olcrtc service when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.configureOlcrtc({ enabled: true, user: 'admin', password: 'pass', port: 8888 }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      done();
    });
  });

  await t.test('ConfigureAwg should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.configureAwg({ enabled: true, port: 51820 }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('ConfigureAwg should write AWG3 config and return success when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const payload = {
      enabled: true,
      port: 51820,
      serverPrivateKey: 'privkey123',
      serverPublicKey: 'pubkey123',
      addressV4: '10.66.66.1/24',
      addressV6: 'fd42:42:42::1/64',
      jc: 4,
      jmin: 40,
      jmax: 70,
      s1: '15',
      s2: '25',
      s3: '35',
      s4: '45',
      h1: '317325099-317328813',
      h2: '202',
      h3: '303',
      h4: '404',
      i1: '<b 0xfe00><rc 16>',
      i2: '<b 0x00><rc 4>',
      i3: '10-20',
      i4: '30',
      i5: '40',
      headerProtectionKey: 'protkey999',
      peers: [
        {
          publicKey: 'peerpubkey1',
          presharedKey: 'psk1',
          allowedIps: '10.66.66.2/32'
        }
      ],
      ipv6Mode: 'dual-stack'
    };

    client.configureAwg(payload, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        const awgContent = await fs.readFile(tempAwgPath, 'utf-8');
        assert.ok(awgContent.includes('PrivateKey = privkey123'));
        assert.ok(awgContent.includes('ListenPort = 51820'));
        assert.ok(awgContent.includes('Address = 10.66.66.1/24, fd42:42:42::1/64'));
        assert.ok(awgContent.includes('Jc = 4'));
        assert.ok(awgContent.includes('Jmin = 40'));
        assert.ok(awgContent.includes('Jmax = 70'));
        assert.ok(awgContent.includes('S1 = 15'));
        assert.ok(awgContent.includes('H1 = 317325099-317328813'));
        assert.ok(awgContent.includes('H2 = 202'));
        assert.ok(awgContent.includes('H3 = 303'));
        assert.ok(awgContent.includes('H4 = 404'));
        assert.ok(awgContent.includes('I1 = <b 0xfe00><rc 16>'));
        assert.ok(awgContent.includes('I2 = <b 0x00><rc 4>'));
        assert.ok(awgContent.includes('I3 = 10-20'));
        assert.ok(awgContent.includes('I4 = 30'));
        assert.ok(awgContent.includes('I5 = 40'));
        assert.ok(awgContent.includes('HeaderProtectionKey = protkey999'));
        assert.ok(awgContent.includes('PublicKey = peerpubkey1'));
        assert.ok(awgContent.includes('PresharedKey = psk1'));
        assert.ok(awgContent.includes('AllowedIPs = 10.66.66.2/32'));
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('ConfigureAwg should return success when disabling service', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.configureAwg({ enabled: false, port: 51820 }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      assert.ok(response.message.includes('disabled and stopped'));
      done();
    });
  });

  await t.test('ManageFirewall should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.manageFirewall({ openUdpPorts: [443], openTcpPorts: [80] }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('ManageFirewall should process ports when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.manageFirewall({ openUdpPorts: [443, 8443], openTcpPorts: [80, 443] }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      done();
    });
  });

  await t.test('SelfUpdate should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.selfUpdate({}, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('SelfUpdate should initiate self-update sequence when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.selfUpdate({}, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      assert.ok(response.message.includes('Self-update sequence initiated'));
      done();
    });
  });

  await t.test('GetAgentInfo should return deterministic proto contract hash and agent info without secret', (t, done) => {
    client.getAgentInfo({ orchestrator_secret: 'test-secret-123' }, new grpc.Metadata(), (err: any, response: any) => {
      try {
        assert.ifError(err);
        const hash = response.protoContractHash || response.proto_contract_hash;
        const version = response.agentVersion || response.agent_version;
        const source = response.protoContractSource || response.proto_contract_source;

        assert.ok(hash, 'proto_contract_hash must be present');
        assert.strictEqual(typeof hash, 'string');
        assert.strictEqual(hash.length, 64, `proto_contract_hash length must be 64, got ${hash.length} (${hash})`);
        assert.match(hash, /^[a-f0-9]{64}$/);

        assert.ok(version, 'agent_version must be present');
        assert.notStrictEqual(hash, version, 'proto_contract_hash and agent_version must not be equal');
        assert.notStrictEqual(hash.length, version.length, 'proto_contract_hash length (64) must not equal agent_version length');
        assert.strictEqual(source, 'canonical-json');

        // Проверяем детерминированность вызова computeProtoContractHash
        const hash1 = computeProtoContractHash();
        const hash2 = computeProtoContractHash();
        assert.strictEqual(hash1, hash2);
        assert.strictEqual(hash, hash1);
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('computeProtoContractHash should change hash when field number or type is modified', async () => {
    clearProtoContractHashCache();
    const baseHash = computeProtoContractHash();

    // Симулируем изменение типа поля в схеме
    const fakePackageDef: any = {
      'agent.TestMsg': {
        format: 'Protocol Buffer 3 DescriptorProto',
        type: {
          field: [
            { number: 1, name: 'fieldA', type: 'TYPE_STRING', label: 'LABEL_OPTIONAL' }
          ]
        }
      }
    };
    const fakePackageDefModified: any = {
      'agent.TestMsg': {
        format: 'Protocol Buffer 3 DescriptorProto',
        type: {
          field: [
            { number: 1, name: 'fieldA', type: 'TYPE_UINT32', label: 'LABEL_OPTIONAL' }
          ]
        }
      }
    };

    const schema1 = getCanonicalSchemaFromPackageDef(fakePackageDef);
    const schema2 = getCanonicalSchemaFromPackageDef(fakePackageDefModified);

    const cryptoModule = await import('crypto');
    const h1 = cryptoModule.createHash('sha256').update(JSON.stringify(schema1)).digest('hex');
    const h2 = cryptoModule.createHash('sha256').update(JSON.stringify(schema2)).digest('hex');
    assert.notStrictEqual(h1, h2);
    assert.ok(baseHash);
  });

  await t.test('computeProtoContractHash should produce identical hash for reordered proto declarations', async () => {
    clearProtoContractHashCache();
    const originalHash = computeProtoContractHash(PROTO_PATH);

    const originalContent = await fs.readFile(PROTO_PATH, 'utf-8');
    const blocks = originalContent.split(/\n(?=message |service )/);
    const reorderedContent = blocks.slice(0, 1).concat(blocks.slice(1).reverse()).join('\n');

    const reorderedProtoPath = path.join(tempDir, 'agent-reordered.proto');
    await fs.writeFile(reorderedProtoPath, reorderedContent, 'utf-8');

    clearProtoContractHashCache();
    const reorderedHash = computeProtoContractHash(reorderedProtoPath);

    assert.strictEqual(reorderedHash, originalHash, 'computeProtoContractHash must produce identical hash regardless of block or field declaration order in .proto');
  });

  await t.test('WebRTC status check logic (mocking olcrtc-manager API)', async (t) => {
    const http = await import('http');
    
    const mockPort = 18888;
    process.env.OLCRTC_PORT = String(mockPort);
    process.env.TEST_WEBRTC_CHECK = 'true';

    let mockResponse: any = {};
    let shouldFail = false;

    const mockServer = http.createServer((req, res) => {
      if (shouldFail) {
        req.destroy();
        return;
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(mockResponse));
    });

    await new Promise<void>((resolve) => mockServer.listen(mockPort, resolve));

    t.after(() => {
      mockServer.close();
      delete process.env.OLCRTC_PORT;
      delete process.env.TEST_WEBRTC_CHECK;
    });

    await t.test('Should return panel_dead if server does not respond or times out', (t, done) => {
      shouldFail = true;
      const stream = client.streamTelemetry({ orchestratorSecret: 'test-secret-123' });
      stream.on('data', (data: any) => {
        try {
          assert.strictEqual(data.webrtcStatus, 'panel_dead');
          stream.destroy();
          done();
        } catch (err) {
          stream.destroy();
          done(err);
        }
      });
      stream.on('error', () => {});
    });

    await t.test('Should return no_active_tunnels if running_count is 0 but there are active users', (t, done) => {
      shouldFail = false;
      mockResponse = { running_count: 0, active_users: 3 };
      const stream = client.streamTelemetry({ orchestratorSecret: 'test-secret-123' });
      stream.on('data', (data: any) => {
        try {
          assert.strictEqual(data.webrtcStatus, 'no_active_tunnels');
          stream.destroy();
          done();
        } catch (err) {
          stream.destroy();
          done(err);
        }
      });
      stream.on('error', () => {});
    });

    await t.test('Should return nominal if running_count > 0 or there are no active users', (t, done) => {
      shouldFail = false;
      mockResponse = { running_count: 5, active_users: 3 };
      const stream = client.streamTelemetry({ orchestratorSecret: 'test-secret-123' });
      stream.on('data', (data: any) => {
        try {
          assert.strictEqual(data.webrtcStatus, 'nominal');
          stream.destroy();
          done();
        } catch (err) {
          stream.destroy();
          done(err);
        }
      });
      stream.on('error', () => {});
    });
  });

  await t.test('mTLS Server & Client Verification', async (t) => {
    const { config } = await import('../src/config.js');

    const originalCa = config.CA_CERT_PATH;
    const originalCert = config.AGENT_CERT_PATH;
    const originalKey = config.AGENT_KEY_PATH;
    const originalPort = config.PORT;

    const certsDir = path.join(__dirname, 'fixtures', 'certs');
    (config as any).CA_CERT_PATH = path.join(certsDir, 'ca.crt');
    (config as any).AGENT_CERT_PATH = path.join(certsDir, 'agent.crt');
    (config as any).AGENT_KEY_PATH = path.join(certsDir, 'agent.key');
    (config as any).PORT = 8083;

    const mtlsServer = await startServer();

    const caCert = await fs.readFile(path.join(certsDir, 'ca.crt'));
    const clientCert = await fs.readFile(path.join(certsDir, 'client.crt'));
    const clientKey = await fs.readFile(path.join(certsDir, 'client.key'));
    const untrustedCert = await fs.readFile(path.join(certsDir, 'untrusted.crt'));
    const untrustedKey = await fs.readFile(path.join(certsDir, 'untrusted.key'));

    t.after(() => {
      mtlsServer.forceShutdown();
      (config as any).CA_CERT_PATH = originalCa;
      (config as any).AGENT_CERT_PATH = originalCert;
      (config as any).AGENT_KEY_PATH = originalKey;
      (config as any).PORT = originalPort;
    });

    await t.test('Valid client mTLS certificate should successfully communicate with agent', (t, done) => {
      const sslCreds = grpc.credentials.createSsl(caCert, clientKey, clientCert);
      const mtlsClient = new EgressAgentService('127.0.0.1:8083', sslCreds);

      const validMetadata = new grpc.Metadata();
      validMetadata.add('x-orchestrator-secret', 'test-secret-123');

      mtlsClient.applyConfig({ configJson: JSON.stringify({ log: { level: 'info' } }) }, validMetadata, (err: any, response: any) => {
        try {
          assert.ifError(err);
          assert.strictEqual(response.success, true);
          mtlsClient.close();
          done();
        } catch (e) {
          mtlsClient.close();
          done(e);
        }
      });
    });

    await t.test('Untrusted client mTLS certificate should be rejected during TLS handshake', (t, done) => {
      const badSslCreds = grpc.credentials.createSsl(caCert, untrustedKey, untrustedCert);
      const badMtlsClient = new EgressAgentService('127.0.0.1:8083', badSslCreds);

      const validMetadata = new grpc.Metadata();
      validMetadata.add('x-orchestrator-secret', 'test-secret-123');

      badMtlsClient.applyConfig({ configJson: '{}' }, validMetadata, (err: any) => {
        try {
          assert.ok(err, 'Expected gRPC call to fail with TLS handshake error');
          badMtlsClient.close();
          done();
        } catch (e) {
          badMtlsClient.close();
          done(e);
        }
      });
    });
  });

  await t.test('getGrpcServerCredentials refuses to start without valid mTLS material (no insecure fallback)', async (t) => {
    const { config } = await import('../src/config.js');

    const originalCa = config.CA_CERT_PATH;
    const originalCert = config.AGENT_CERT_PATH;
    const originalKey = config.AGENT_KEY_PATH;

    t.after(() => {
      (config as any).CA_CERT_PATH = originalCa;
      (config as any).AGENT_CERT_PATH = originalCert;
      (config as any).AGENT_KEY_PATH = originalKey;
    });

    await t.test('Should throw (not fall back to insecure) when certificate files are missing', async () => {
      const missingDir = path.join(tempDir, 'nonexistent-certs');
      (config as any).CA_CERT_PATH = path.join(missingDir, 'ca.crt');
      (config as any).AGENT_CERT_PATH = path.join(missingDir, 'agent.crt');
      (config as any).AGENT_KEY_PATH = path.join(missingDir, 'agent.key');

      await assert.rejects(
        () => getGrpcServerCredentials(),
        (err: unknown) => {
          assert.ok(err instanceof Error);
          assert.match(err.message, /mTLS certificates are required but unavailable/);
          assert.match(err.message, /MISSING at/);
          assert.match(err.message, /refuses to start/);
          return true;
        }
      );
    });

    await t.test('Should throw (not fall back to insecure) when a cert path exists but cannot be read as a file', async () => {
      const corruptDir = path.join(tempDir, 'corrupt-certs');
      await fs.mkdir(corruptDir, { recursive: true });
      const caPath = path.join(corruptDir, 'ca.crt');
      const certPath = path.join(corruptDir, 'agent.crt');
      // AGENT_KEY_PATH указывает на директорию: fs.stat находит "файл" (проходит проверку
      // существования), но fs.readFile на директории бросает EISDIR — это должно всплыть
      // как ошибка загрузки, а не тихий insecure-фоллбек
      const keyPath = path.join(corruptDir, 'agent-key-is-a-dir');
      await fs.mkdir(keyPath, { recursive: true });
      await Promise.all([
        fs.writeFile(caPath, 'not a real certificate'),
        fs.writeFile(certPath, 'not a real certificate'),
      ]);
      (config as any).CA_CERT_PATH = caPath;
      (config as any).AGENT_CERT_PATH = certPath;
      (config as any).AGENT_KEY_PATH = keyPath;

      await assert.rejects(
        () => getGrpcServerCredentials(),
        (err: unknown) => {
          assert.ok(err instanceof Error);
          assert.match(err.message, /mTLS certificates were found but failed to load/);
          assert.match(err.message, /refuses to start/);
          return true;
        }
      );
    });
  });

  await t.test('Middleware and Utilities Unit Tests', async (t) => {
    const { verifySecret, authenticateCall } = await import('../src/middleware/auth.js');
    const { validateSafeCamouflagePath } = await import('../src/utils/caddy.js');
    const { execAsync, execFileAsync } = await import('../src/utils/exec.js');

    await t.test('verifySecret should use timingSafeEqual and return true for valid secret', () => {
      assert.strictEqual(verifySecret('test-secret-123'), true);
      assert.strictEqual(verifySecret('wrong-secret'), false);
      assert.strictEqual(verifySecret(''), false);
      assert.strictEqual(verifySecret(null as any), false);
      assert.strictEqual(verifySecret(undefined as any), false);
    });

    await t.test('authenticateCall should check metadata and request payload', () => {
      const mockCallWithMetadata = {
        metadata: {
          get: (key: string) => key === 'x-orchestrator-secret' ? ['test-secret-123'] : []
        }
      };
      assert.strictEqual(authenticateCall(mockCallWithMetadata), true);

      const mockCallWithPayload = {
        request: { orchestratorSecret: 'test-secret-123' }
      };
      assert.strictEqual(authenticateCall(mockCallWithPayload), true);

      const mockCallInvalid = {
        request: { orchestratorSecret: 'invalid' }
      };
      assert.strictEqual(authenticateCall(mockCallInvalid), false);
    });

    await t.test('validateSafeCamouflagePath should allow safe paths and reject Path Traversal attempts', () => {
      const safe1 = validateSafeCamouflagePath('/var/www/my-site');
      assert.ok(safe1.endsWith('my-site'));

      const safe2 = validateSafeCamouflagePath('/tmp/camouflage/site');
      assert.ok(safe2.endsWith('site'));

      assert.throws(() => {
        validateSafeCamouflagePath('/var/www/../etc/passwd');
      }, /Path Traversal restriction/);

      assert.throws(() => {
        validateSafeCamouflagePath('/root/secret');
      }, /Path Traversal restriction/);
    });

    await t.test('execAsync and execFileAsync should be functions', () => {
      assert.strictEqual(typeof execAsync, 'function');
      assert.strictEqual(typeof execFileAsync, 'function');
    });

    await t.test('replaceBinaryAtomically should atomically replace target file', async () => {
      const { replaceBinaryAtomically } = await import('../src/services/binary.service.js');
      const srcFile = path.join(tempDir, 'test_src_binary.tmp');
      const targetFile = path.join(tempDir, 'test_target_dir', 'test_binary');

      await fs.writeFile(srcFile, 'binary_content_v1');
      await replaceBinaryAtomically(srcFile, targetFile);

      const targetContent = await fs.readFile(targetFile, 'utf-8');
      assert.strictEqual(targetContent, 'binary_content_v1');

      // Verify overwrite of existing binary
      const srcFile2 = path.join(tempDir, 'test_src_binary_2.tmp');
      await fs.writeFile(srcFile2, 'binary_content_v2');
      await replaceBinaryAtomically(srcFile2, targetFile);

      const updatedContent = await fs.readFile(targetFile, 'utf-8');
      assert.strictEqual(updatedContent, 'binary_content_v2');
    });

    await t.test('fixCaddyPermissions should chown before chmod, and skip both if the dir does not exist', async () => {
      const { fixCaddyPermissions } = await import('../src/utils/singbox.js');

      // Существующая директория: обе команды должны выполниться, chown первой.
      const existingDir = path.join(tempDir, 'fake_var_lib_caddy');
      await fs.mkdir(existingDir, { recursive: true });

      const calls: string[] = [];
      const spyExec = async (command: string) => {
        calls.push(command);
        return { stdout: '', stderr: '' };
      };

      await fixCaddyPermissions(existingDir, spyExec);

      assert.strictEqual(calls.length, 2, 'expected exactly one chown call and one chmod call');
      assert.match(calls[0], /^chown -R caddy:caddy /, 'first command must be chown, not just chmod');
      assert.ok(calls[0].includes(existingDir));
      assert.match(calls[1], /^chmod -R 755 /);
      assert.ok(calls[1].includes(existingDir));

      // Несуществующая директория: exec вообще не должен вызываться.
      const missingCalls: string[] = [];
      const spyExecMissing = async (command: string) => {
        missingCalls.push(command);
        return { stdout: '', stderr: '' };
      };
      await fixCaddyPermissions(path.join(tempDir, 'does_not_exist_caddy_dir'), spyExecMissing);
      assert.strictEqual(missingCalls.length, 0);
    });

    await t.test('parseAwgVersionString, getAwgToolsVersion, and getAmneziaWgGoVersion logic', async () => {
      const { parseAwgVersionString, getAwgToolsVersion, getAmneziaWgGoVersion, getAwgVersion } = await import('../src/utils/telemetry.js');

      assert.strictEqual(parseAwgVersionString('awg v1.0.20260618-2\n'), '1.0.20260618-2');
      assert.strictEqual(parseAwgVersionString('v1.0.0'), '1.0.0');
      assert.strictEqual(parseAwgVersionString('v3.0.2'), '3.0.2');
      assert.strictEqual(parseAwgVersionString('0.0.20250522'), '0.0.20250522');
      assert.strictEqual(parseAwgVersionString('1.0.20260618-2'), '1.0.20260618-2');
      assert.strictEqual(parseAwgVersionString('amneziawg-go v3.0.2'), '3.0.2');
      assert.strictEqual(parseAwgVersionString('amneziawg-go v0.0.20230223'), '0.0.20230223');
      assert.strictEqual(parseAwgVersionString('no_version_here'), null);

      process.env.TEST_AWG_TOOLS_VERSION = '3.0.2';
      assert.strictEqual(await getAwgToolsVersion(), '3.0.2');
      assert.strictEqual(await getAwgVersion(), '3.0.2');

      process.env.TEST_AMNEZIAWG_GO_VERSION = '0.0.20250522';
      assert.strictEqual(await getAmneziaWgGoVersion(), '0.0.20250522');

      delete process.env.TEST_AWG_TOOLS_VERSION;
      delete process.env.TEST_AMNEZIAWG_GO_VERSION;
    });

    await t.test('getAwgStatus test logic', async () => {
      const { getAwgStatus } = await import('../src/utils/telemetry.js');

      process.env.TEST_AWG_STATUS = 'nominal';
      assert.strictEqual(await getAwgStatus(), 'nominal');

      process.env.TEST_AWG_STATUS = 'inactive';
      assert.strictEqual(await getAwgStatus(), 'inactive');

      delete process.env.TEST_AWG_STATUS;
    });
  });
});


