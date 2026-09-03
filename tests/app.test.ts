// tests/app.test.ts

import test from 'node:test';
import assert from 'node:assert';
import { promises as fs } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import * as http from 'node:http';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { computeProtoContractHash, clearProtoContractHashCache, getCanonicalSchemaFromPackageDef } from '../src/services/protoContract.service.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const tempDir = path.join(__dirname, 'temp');
const tempConfigPath = path.join(tempDir, 'sing-box-config.json');
const tempBinaryPath = path.join(tempDir, 'sing-box');
const tempCaddyfilePath = path.join(tempDir, 'Caddyfile');
const tempOlcrtcAgentSrvPath = path.join(tempDir, 'olcrtc-agent-srv');
const tempOlcrtcAgentSrvUnitPath = path.join(tempDir, 'olcrtc-agent-srv@.service');
const tempOlcrtcAgentSrvConfigDir = path.join(tempDir, 'olcrtc-agent-srv-config');
const tempAwgPath = path.join(tempDir, 'awg0.conf');

const tempAwgToolsBinaryPath = path.join(tempDir, 'awg');
const tempAwgQuickBinaryPath = path.join(tempDir, 'awg-quick');
const tempAwgGoBinaryPath = path.join(tempDir, 'amneziawg-go');
const tempAwgUnitPath = path.join(tempDir, 'route-awg@.service');
const tempSingboxUnitPath = path.join(tempDir, 'sing-box.service');
const tempRearConfigPath = path.join(tempDir, 'rear.json');
const tempRearUnitPath = path.join(tempDir, 'route-rear-singbox.service');
const tempMeshAwgPath = path.join(tempDir, 'awgmesh0.conf');

// Фикстурные mTLS-сертификаты для тестов. С тех пор, как агент отказывается стартовать
// без валидных сертификатов (см. security-аудит), даже "обычный" пайплайн-тест обязан
// поднимать сервер в mTLS-режиме — insecure-режима у агента больше не существует.
const fixtureCertsDir = path.join(__dirname, 'fixtures', 'certs');

// Конфигурируем тестовое окружение до загрузки модулей
process.env.NODE_ENV = 'test';
// ФИКСИРОВАННЫЙ порт, и из-за него весь набор агента гоняется с --test-concurrency=1 (см.
// package.json). Раннер запускает по процессу на файл, так что два файла с этим портом
// одновременно подрались бы за bind. У агента файлов всего три, сериализация стоит секунды —
// поэтому проще оставить флаг, чем переводить на эфемерный порт.
//
// В оркестраторе и вотчере того же флага НЕТ и быть не должно: там все порты эфемерные
// (listen(0)), временные каталоги через mkdtemp, а файлов 280 — сериализация стоила там 156
// секунд на каждом прогоне (186 с против 30 с, замерено 2026-09-01). Если соберёшься добавить
// флаг туда «на всякий случай» — не надо, сначала найди настоящую общую ресурсную зависимость.
process.env.PORT = '8082';
process.env.HOST = '127.0.0.1';
process.env.EGRESS_CONTROL_SECRET = 'test-secret-123';
process.env.CA_CERT_PATH = path.join(fixtureCertsDir, 'ca.crt');
process.env.AGENT_CERT_PATH = path.join(fixtureCertsDir, 'agent.crt');
process.env.AGENT_KEY_PATH = path.join(fixtureCertsDir, 'agent.key');
process.env.SINGBOX_CONFIG_PATH = tempConfigPath;
process.env.SINGBOX_BINARY_PATH = tempBinaryPath;
process.env.CADDYFILE_PATH = tempCaddyfilePath;
process.env.OLCRTC_AGENT_SRV_BINARY_PATH = tempOlcrtcAgentSrvPath;
process.env.OLCRTC_AGENT_SRV_UNIT_FILE_PATH = tempOlcrtcAgentSrvUnitPath;
process.env.OLCRTC_AGENT_SRV_CONFIG_DIR = tempOlcrtcAgentSrvConfigDir;
process.env.AWG_CONFIG_PATH = tempAwgPath;
process.env.AWG_TOOLS_BINARY_PATH = tempAwgToolsBinaryPath;
process.env.AWG_QUICK_BINARY_PATH = tempAwgQuickBinaryPath;
process.env.AWG_GO_BINARY_PATH = tempAwgGoBinaryPath;
process.env.AWG_UNIT_FILE_PATH = tempAwgUnitPath;
process.env.MESH_AWG_CONFIG_PATH = tempMeshAwgPath;
process.env.SINGBOX_UNIT_FILE_PATH = tempSingboxUnitPath;
process.env.REAR_SINGBOX_CONFIG_PATH = tempRearConfigPath;
process.env.REAR_SINGBOX_UNIT_FILE_PATH = tempRearUnitPath;
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
  // grpc.ssl_target_name_override: Node's TLS module now rejects an IP-literal SNI/servername
  // outright ("Setting the TLS ServerName to an IP address is not permitted"), so the target
  // string can't be the literal '127.0.0.1' used to dial. The fixture cert's SAN covers both
  // IP:127.0.0.1 and DNS:localhost — overriding just the TLS handshake's name to 'localhost'
  // (not the actual dial target, which stays 127.0.0.1) satisfies both that restriction and
  // hostname verification, without depending on how this OS resolves the literal 'localhost'.
  const client = new EgressAgentService(
    '127.0.0.1:8082',
    grpc.credentials.createSsl(caCert, clientKey, clientCert),
    { 'grpc.ssl_target_name_override': 'localhost', 'grpc.default_authority': 'localhost' }
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


  // --- ConfigureRearSingbox: тыловой инстанс sing-box (WARP-выход) ---------------------------
  //
  // Второй процесс sing-box на той же ноде, из ТОГО ЖЕ бинаря. Конфиг для него целиком собирает
  // оркестратор: в нём приватные ключи WARP из его пула.

  await t.test('ConfigureRearSingbox should block requests with invalid metadata tokens', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'malicious_token');

    client.configureRearSingbox({ enabled: true, configJson: '{}' }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('ConfigureRearSingbox should write the config and the unit when enabled', async () => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const payload = { log: { level: 'warn' }, inbounds: [], outbounds: [{ type: 'direct', tag: 'direct' }] };

    const response: any = await new Promise((resolve, reject) => {
      client.configureRearSingbox(
        { enabled: true, config_json: JSON.stringify(payload), configJson: JSON.stringify(payload) },
        validMetadata,
        (err: any, res: any) => (err ? reject(err) : resolve(res))
      );
    });

    assert.strictEqual(response.success, true);

    const written = JSON.parse(await fs.readFile(tempRearConfigPath, 'utf-8'));
    assert.deepStrictEqual(written, payload, 'конфиг тыла записан не тем, что прислал оркестратор');
  });

  await t.test('the rear unit runs the SAME sing-box binary as the front instance', async () => {
    // Один бинарь на два инстанса — намеренно: так UpgradeSingbox и SelfUpdate покрывают оба, и
    // не появляется второго пути установки, о версии которого никто не знает. Отдельный бинарь в
    // ExecStart тихо развалил бы это свойство.
    const unit = await fs.readFile(tempRearUnitPath, 'utf-8');

    assert.ok(unit.includes(`ExecStart=${tempBinaryPath} run -c ${tempRearConfigPath}`), unit);
    assert.ok(unit.includes('Restart=on-failure'));

    // RELOAD-путь, и он повторяет форму фронтового юнита (`utils/singbox.ts`): проверка конфига
    // ВНУТРИ перезагрузки, SIGHUP только при её успехе. Бесшовности SIGHUP не даёт — sing-box
    // закрывает инстанс целиком, — но негодный конфиг при таком ExecReload не роняет тыл вовсе.
    assert.ok(
      unit.includes(`ExecReload=/bin/sh -c "${tempBinaryPath} check -c ${tempRearConfigPath} && /bin/kill -HUP $MAINPID"`),
      unit
    );
  });

  await t.test('ConfigureRearSingbox should not rewrite the unit when nothing changed', async () => {
    // Иначе каждый пуш конфига дёргал бы daemon-reload без причины (тот же приём, что в
    // ensureAwgSystemdUnit).
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');
    const before = (await fs.stat(tempRearUnitPath)).mtimeMs;

    await new Promise((resolve, reject) => {
      client.configureRearSingbox(
        { enabled: true, configJson: JSON.stringify({ log: { level: 'warn' } }) },
        validMetadata,
        (err: any, res: any) => (err ? reject(err) : resolve(res))
      );
    });

    assert.strictEqual((await fs.stat(tempRearUnitPath)).mtimeMs, before, 'unit переписан без изменений');
  });

  await t.test('ConfigureRearSingbox should remove the config and unit when disabled', async () => {
    /**
     * Конфиг удаляется вместе с инстансом не для чистоты: в нём лежат ПРИВАТНЫЕ КЛЮЧИ WARP.
     * Оставить их на ноде, выведенной из пула, значило бы держать рабочий доступ к аккаунтам
     * Cloudflare там, где он больше никому не нужен.
     */
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const response: any = await new Promise((resolve, reject) => {
      client.configureRearSingbox({ enabled: false, configJson: '' }, validMetadata, (err: any, res: any) =>
        err ? reject(err) : resolve(res)
      );
    });

    assert.strictEqual(response.success, true);
    assert.strictEqual(response.running, false);
    assert.strictEqual(await fs.stat(tempRearConfigPath).then(() => true).catch(() => false), false);
    assert.strictEqual(await fs.stat(tempRearUnitPath).then(() => true).catch(() => false), false);
  });


  // --- GetWarpKeyHealth: здоровье WARP-ключей, снятое у тылового sing-box --------------------

  await t.test('GetWarpKeyHealth should block requests with invalid metadata tokens', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'malicious_token');

    client.getWarpKeyHealth({}, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('GetWarpKeyHealth should say rear_not_running when there is no rear config', async () => {
    /**
     * Отличается от «тыл есть, но молчит» намеренно: на ноде без включённого WARP это НОРМАЛЬНОЕ
     * состояние, и оркестратор не должен по нему ни шуметь, ни списывать ключи.
     */
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');
    await fs.rm(tempRearConfigPath, { force: true });

    const response: any = await new Promise((resolve, reject) => {
      client.getWarpKeyHealth({}, validMetadata, (err: any, res: any) => (err ? reject(err) : resolve(res)));
    });

    assert.strictEqual(response.success, true);
    assert.strictEqual(response.skippedReason, 'rear_not_running');
    // Тот же нюанс загрузчика: без defaults:true пустой repeated приезжает undefined, а не [].
    // На стороне оркестратора (defaults:true) это будет [], и воркер там всё равно подставляет пустой список.
    assert.deepStrictEqual(response.entries ?? [], []);
  });

  await t.test('GetWarpKeyHealth should read measurements from the rear clash api', async () => {
    /**
     * Замеры делает сам sing-box: конфиг тыла собирает WARP-туннели в группу `urltest`, и Clash API
     * отдаёт уже накопленную историю. Здесь этот API подменён локальным сервером, но форма ответа
     * взята из исходников sing-box v1.14.0 — `proxies` -> `{ history: [{ time, delay }] }`, причём
     * endpoint-ы попадают в тот же список, что и outbound-ы.
     */
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const fakeClash = http.createServer((req, res) => {
      res.setHeader('content-type', 'application/json');
      res.end(
        JSON.stringify({
          proxies: {
            direct: { history: [] },
            warp: { history: [{ time: '2026-09-03T04:00:00.000Z', delay: 30 }] },
            'warp-key-alive': { history: [{ time: '2026-09-03T04:00:00.000Z', delay: 91 }] },
            'warp-key-dead': { history: [] },
          },
        })
      );
    });
    await new Promise<void>((resolve) => fakeClash.listen(0, '127.0.0.1', resolve));
    const port = (fakeClash.address() as { port: number }).port;

    try {
      await fs.writeFile(
        tempRearConfigPath,
        JSON.stringify({ experimental: { clash_api: { external_controller: `127.0.0.1:${port}`, secret: 'x' } } }),
        'utf-8'
      );

      const response: any = await new Promise((resolve, reject) => {
        client.getWarpKeyHealth({}, validMetadata, (err: any, res: any) => (err ? reject(err) : resolve(res)));
      });

      assert.strictEqual(response.success, true);
      // Клиент в этом файле грузит proto БЕЗ defaults:true, поэтому незаполненная строка
      // приезжает undefined, а в проде (defaults:true с обеих сторон) — пустой строкой. Проверяем
      // отсутствие причины, а не её конкретное представление.
      assert.ok(!response.skippedReason, String(response.skippedReason));

      const byTag = new Map<string, any>(response.entries.map((e: any) => [e.endpointTag, e]));
      assert.deepStrictEqual([...byTag.keys()].sort(), ['warp-key-alive', 'warp-key-dead']);
      assert.strictEqual(byTag.get('warp-key-alive').alive, true);
      assert.strictEqual(byTag.get('warp-key-alive').rttMs, 91);
      // Пустая история — это «не ответил», а не «нет данных»: sing-box УДАЛЯЕТ запись при неудаче.
      assert.strictEqual(byTag.get('warp-key-dead').alive, false);
    } finally {
      await new Promise<void>((resolve) => fakeClash.close(() => resolve()));
      await fs.rm(tempRearConfigPath, { force: true });
    }
  });

  await t.test('GetWarpKeyHealth should say clash_api_unreachable when the rear API does not answer', async () => {
    // Тыл настроен, но его API молчит — вот это уже повод посмотреть, и оркестратор должен уметь
    // отличить этот случай от «WARP на ноде просто не включён».
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    // Порт, который заведомо никто не слушает: занимаем и сразу освобождаем.
    const probe = http.createServer();
    await new Promise<void>((resolve) => probe.listen(0, '127.0.0.1', resolve));
    const deadPort = (probe.address() as { port: number }).port;
    await new Promise<void>((resolve) => probe.close(() => resolve()));

    try {
      await fs.writeFile(
        tempRearConfigPath,
        JSON.stringify({ experimental: { clash_api: { external_controller: `127.0.0.1:${deadPort}`, secret: '' } } }),
        'utf-8'
      );

      const response: any = await new Promise((resolve, reject) => {
        client.getWarpKeyHealth({}, validMetadata, (err: any, res: any) => (err ? reject(err) : resolve(res)));
      });

      assert.strictEqual(response.success, false);
      assert.strictEqual(response.skippedReason, 'clash_api_unreachable');
    } finally {
      await fs.rm(tempRearConfigPath, { force: true });
    }
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

  await t.test('UploadOlcrtcAgentSrvBinary should upload the olcrtc-agent-srv binary', (t, done) => {
    const call = client.uploadOlcrtcAgentSrvBinary(validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('olcrtc-agent-srv'));
        const exists = await fs.stat(tempOlcrtcAgentSrvPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, true);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('olcrtc_agent_srv_chunk'), version: '0.1.0', isFinal: true });
    call.end();
  });

  await t.test('UploadOlcrtcAgentSrvBinary should reject unauthorized uploads', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    const call = client.uploadOlcrtcAgentSrvBinary(badMetadata, (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, false);
        done();
      } catch (e) {
        done(e);
      }
    });
    call.write({ orchestratorSecret: 'wrong-secret', chunk: Buffer.from('nope'), version: '0.1.0', isFinal: true });
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

  await t.test('ensureOlcrtcAgentSrvSystemdUnit idempotency check (mirrors ensureAwgSystemdUnit)', async () => {
    const { ensureOlcrtcAgentSrvSystemdUnit } = await import('../src/services/olcrtc.service.js');
    const customTestUnitPath = path.join(tempDir, 'test-olcrtc-agent-srv@.service');
    await fs.unlink(customTestUnitPath).catch(() => {});

    const firstCallResult = await ensureOlcrtcAgentSrvSystemdUnit(customTestUnitPath);
    assert.strictEqual(firstCallResult, true, 'First call should create unit file');

    const content = await fs.readFile(customTestUnitPath, 'utf-8');
    assert.ok(content.includes('Description=olcrtc-agent-srv instance %i (managed by route-agent)'));
    assert.ok(content.includes(`ExecStart=${tempOlcrtcAgentSrvPath} ${tempOlcrtcAgentSrvConfigDir}/%i.yaml`));
    assert.ok(content.includes('Restart=always'));

    const secondCallResult = await ensureOlcrtcAgentSrvSystemdUnit(customTestUnitPath);
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

  await t.test('resolveCaddyBinary falls back to bare "caddy" when the custom binary is not present on disk, and resolves the full path when it is', async () => {
    const { resolveCaddyBinary } = await import('../src/utils/caddy.js');
    const { config } = await import('../src/config.js');

    const originalCaddyBinaryPath = (config as any).CADDY_BINARY_PATH;
    const fakeCustomPath = path.join(tempDir, 'caddy-custom-fake');
    (config as any).CADDY_BINARY_PATH = fakeCustomPath;

    try {
      await fs.unlink(fakeCustomPath).catch(() => {});
      // Regression guard for the real prod bug: the agent's own env var is fixed at process
      // start (dotenv, loaded once) but uploadCaddyBinaryHandler writes the custom binary to
      // disk mid-lifetime — an env-var-only check would never see it without restarting the
      // agent itself. resolveCaddyBinary must check disk presence at CALL time instead.
      const beforeUpload = await resolveCaddyBinary();
      assert.strictEqual(beforeUpload, 'caddy', 'must fall back to bare caddy before the custom binary exists on disk');

      await fs.writeFile(fakeCustomPath, 'fake binary content');
      const afterUpload = await resolveCaddyBinary();
      assert.strictEqual(afterUpload, fakeCustomPath, 'must resolve to the full custom path once it exists on disk, no agent restart needed');
    } finally {
      (config as any).CADDY_BINARY_PATH = originalCaddyBinaryPath;
      await fs.unlink(fakeCustomPath).catch(() => {});
    }
  });

  await t.test('loadCaddyDnsProviderEnv parses KEY=value lines and returns {} when the file is missing', async () => {
    const { loadCaddyDnsProviderEnv } = await import('../src/utils/caddy.js');
    const envPath = path.join(tempDir, 'cloudflare.env');
    await fs.unlink(envPath).catch(() => {});

    // Regression guard: EnvironmentFile= in the systemd override only reaches processes systemd
    // itself starts — an ad-hoc `caddy validate` spawned via execAsync never sees CF_API_TOKEN
    // unless this is explicitly merged into that child process's own env.
    const missing = await loadCaddyDnsProviderEnv(envPath);
    assert.deepEqual(missing, {}, 'missing file must yield an empty object, not throw');

    await fs.writeFile(envPath, '# comment\nCF_API_TOKEN=real-token-value\n\nEMPTY_LINE_ABOVE=ignored\n');
    const parsed = await loadCaddyDnsProviderEnv(envPath);
    assert.deepEqual(parsed, { CF_API_TOKEN: 'real-token-value', EMPTY_LINE_ABOVE: 'ignored' });
  });

  await t.test('ensureCaddyCustomBinaryOverride writes the expected drop-in and is idempotent on a second call', async () => {
    const { ensureCaddyCustomBinaryOverride } = await import('../src/utils/caddy.js');
    const overridePath = path.join(tempDir, 'caddy.service.d', 'override.conf');
    await fs.rm(path.dirname(overridePath), { recursive: true, force: true }).catch(() => {});

    const execCalls: string[] = [];
    const spyExec = async (command: string) => {
      execCalls.push(command);
      return { stdout: '', stderr: '' };
    };

    // Regression guard for a real prod incident: daemon-reload alone does NOT switch an
    // already-running Type=notify unit onto the new ExecStart — only a full restart re-execs
    // under it. The exec calls happen to be skipped entirely under NODE_ENV=test (same
    // limitation ensureSingboxSystemdUnit's own test above already accepts), so this asserts
    // the file CONTENT — the actual restart-vs-reload behavior is exercised by reading
    // ensureCaddyCustomBinaryOverride's source, not re-derivable from a live systemd here.
    const firstCallResult = await ensureCaddyCustomBinaryOverride(overridePath, spyExec);
    assert.strictEqual(firstCallResult, true, 'First call should create the override file');

    const content = await fs.readFile(overridePath, 'utf-8');
    assert.ok(content.includes('ExecStart=\n'), 'must reset ExecStart before overriding it');
    assert.ok(content.includes('caddy-custom'));
    assert.ok(content.includes('EnvironmentFile=-/etc/caddy/cloudflare.env'));

    const secondCallResult = await ensureCaddyCustomBinaryOverride(overridePath, spyExec);
    assert.strictEqual(secondCallResult, false, 'Second call with identical content must return false');
    assert.strictEqual(execCalls.length, 0, 'daemon-reload/restart must not run under NODE_ENV=test');
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

  await t.test('SyncOlcrtcInstance should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.syncOlcrtcInstance({ userShortUuid: 'abc123', carrier: 'jitsi', transport: 'datachannel', roomId: 'https://meet.egovm.ru/x', cryptoKeyHex: '00'.repeat(32) }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('SyncOlcrtcInstance should reject an unsafe/empty user_short_uuid', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.syncOlcrtcInstance({ userShortUuid: '../../etc/passwd', carrier: 'jitsi', transport: 'datachannel', roomId: 'x', cryptoKeyHex: '00'.repeat(32) }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('SyncOlcrtcInstance should write the expected YAML config when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const userShortUuid = 'test-user-1';
    client.syncOlcrtcInstance({
      userShortUuid, carrier: 'jitsi', transport: 'vp8channel',
      roomId: 'https://meet.egovm.ru/testroom', cryptoKeyHex: '11'.repeat(32),
      dns: '8.8.8.8:53', vp8Fps: 60, vp8BatchSize: 64,
    }, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        const configPath = path.join(tempOlcrtcAgentSrvConfigDir, `${userShortUuid}.yaml`);
        const content = await fs.readFile(configPath, 'utf-8');
        assert.match(content, /mode: srv/);
        assert.match(content, /provider: jitsi/);
        assert.match(content, /id: "https:\/\/meet\.egovm\.ru\/testroom"/);
        assert.match(content, /key: "1{64}"/);
        assert.match(content, /transport: vp8channel/);
        assert.match(content, /fps: 60/);
        assert.match(content, /batch_size: 64/);
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('DeleteOlcrtcInstance should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.deleteOlcrtcInstance({ userShortUuid: 'test-user-1' }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('DeleteOlcrtcInstance should remove the instance config file when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const userShortUuid = 'test-user-1';
    const configPath = path.join(tempOlcrtcAgentSrvConfigDir, `${userShortUuid}.yaml`);

    client.deleteOlcrtcInstance({ userShortUuid }, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        const exists = await fs.stat(configPath).then(() => true).catch(() => false);
        assert.strictEqual(exists, false);
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('StreamOlcrtcEvents should reject an unauthenticated stream', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    let settled = false;
    const finish = (err?: Error) => {
      if (settled) return;
      settled = true;
      done(err);
    };

    const stream = client.streamOlcrtcEvents({}, badMetadata);
    stream.on('data', () => {
      finish(new Error('should not receive data on an unauthenticated stream'));
    });
    stream.on('error', (err: any) => {
      assert.ok(err);
      finish();
    });
    stream.on('end', () => {
      finish(new Error('stream ended without an error for an unauthenticated call'));
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

  await t.test('UploadMeshAwgKernelSource should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    const call = client.uploadMeshAwgKernelSource(badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
    call.write({ orchestratorSecret: 'bad_secret', chunk: Buffer.from('irrelevant'), isFinal: true });
    call.end();
  });

  await t.test('UploadMeshAwgKernelSource extracts the tarball and parses PACKAGE_NAME/PACKAGE_VERSION from dkms.conf', async () => {
    // Real gzip tarball fixture shaped like the actual upstream layout (<repo>/src/dkms.conf) —
    // NODE_ENV=test short-circuits the actual apt-get/dkms/modprobe sequence (see
    // meshTunnel.service.ts's installDkmsKernelModule), so this exercises the real
    // extract+parse path without needing a real Linux DKMS toolchain in CI.
    const fixturePath = path.join(__dirname, 'fixtures', 'amneziawg-kernel-src-fixture.tar.gz');
    const fixtureBytes = await fs.readFile(fixturePath);

    const response: any = await new Promise((resolve, reject) => {
      const call = client.uploadMeshAwgKernelSource(validMetadata, (err: any, res: any) => {
        if (err) reject(err); else resolve(res);
      });
      call.write({ orchestratorSecret: 'test-secret-123', chunk: fixtureBytes, isFinal: true });
      call.end();
    });

    // Windows dev boxes: the bundled bsdtar's legacy "host:file" remote-archive heuristic
    // mis-parses a Windows drive-letter colon in the -C target path (a purely local tar.exe
    // quirk with no bearing on the real Linux production target this agent runs on — same class
    // of platform gap already tolerated elsewhere in this test file for `bash -n`, see
    // uploadAwgToolsBinaryHandler's awg-quick syntax check) — only assert the pipeline round-
    // trips a defined response there, and assert the real extract+parse result everywhere else.
    if (process.platform === 'win32') {
      assert.ok(response && typeof response.success === 'boolean');
    } else {
      assert.strictEqual(response.success, true);
      assert.ok(response.message.includes('amneziawg'));
      assert.ok(response.message.includes('9.9.9-test'));
    }
  });

  await t.test('ConfigureMeshTunnel should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.configureMeshTunnel({ privateKey: 'x', listenPort: 51821 }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
  });

  await t.test('ConfigureMeshTunnel should write mesh AWG config with full-mesh peer list and return success', (t, done) => {
    const payload = {
      privateKey: 'meshprivkey123',
      addressV4: '100.100.0.4/16',
      addressV6: 'fd00:a002::4/64',
      listenPort: 51821,
      jc: 4,
      jmin: 40,
      jmax: 70,
      s1: '113', s2: '87', s3: '0', s4: '0',
      h1: '1234567890', h2: '2345678901', h3: '3456789012', h4: '4567890123',
      headerProtectionKey: 'test-header-protection-key',
      i1: 'i1-junk-payload', i2: 'i2-junk-payload', i3: '', i4: '', i5: '',
      peers: [
        { publicKey: 'frontpubkey1', allowedIps: '100.100.0.1/32', endpoint: '203.0.113.1:51821', persistentKeepalive: 25 },
        { publicKey: 'frontpubkey2', allowedIps: '100.100.0.2/32', endpoint: '203.0.113.2:51821', persistentKeepalive: 25 }
      ]
    };

    client.configureMeshTunnel(payload, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('2 peer'));
        const meshContent = await fs.readFile(tempMeshAwgPath, 'utf-8');
        assert.ok(meshContent.includes('PrivateKey = meshprivkey123'));
        assert.ok(meshContent.includes('ListenPort = 51821'));
        assert.ok(meshContent.includes('Address = 100.100.0.4/16, fd00:a002::4/64'));
        assert.ok(meshContent.includes('Jc = 4'));
        // Global mesh mimicry template fields (Task #39) — same [Interface]-level params as
        // client-facing ConfigureAwg, must round-trip into the mesh interface's own config file.
        assert.ok(meshContent.includes('S1 = 113'));
        assert.ok(meshContent.includes('S4 = 0'));
        assert.ok(meshContent.includes('H1 = 1234567890'));
        assert.ok(meshContent.includes('H4 = 4567890123'));
        assert.ok(meshContent.includes('HeaderProtectionKey = test-header-protection-key'));
        assert.ok(meshContent.includes('I1 = i1-junk-payload'));
        assert.ok(meshContent.includes('I2 = i2-junk-payload'));
        assert.ok(!meshContent.includes('I3 ='), 'empty i3/i4/i5 must be omitted, not written as blank lines');
        assert.ok(meshContent.includes('PublicKey = frontpubkey1'));
        assert.ok(meshContent.includes('Endpoint = 203.0.113.1:51821'));
        assert.ok(meshContent.includes('PersistentKeepalive = 25'));
        assert.ok(meshContent.includes('PublicKey = frontpubkey2'));
        done();
      } catch (e) {
        done(e);
      }
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

  // Watcher v2 "Track B-lite" — GetSingBoxUserTraffic RPC. Живого sing-box здесь нет, поэтому
  // покрыто ровно то, что проверяется без него: отказ по токену и две ветки, где спросить нечего
  // или некого. Сам подсчёт приращения — единственная содержательная логика — живёт в
  // tests/singboxStats.test.ts.
  //
  // 2026-09-01: канал переведён с v2ray_api на Clash API. Прежние тесты этого места писали в
  // конфиг блок `v2ray_api` и ждали, что агент его найдёт; менять в них имя блока было бы
  // бессмысленно — они проверяли разбор формата имён статистики v2ray_api, которого больше нет
  // вовсе. Заменены, а не переименованы.
  await t.test('GetSingBoxUserTraffic should block requests with invalid metadata tokens', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.getSingBoxUserTraffic({}, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      assert.strictEqual((response.entries ?? []).length, 0);
      done();
    });
  });

  await t.test('GetSingBoxUserTraffic returns success with no entries when the config has no clash_api block', async () => {
    await fs.writeFile(tempConfigPath, JSON.stringify({ log: { level: 'info' } }), 'utf-8');

    await new Promise<void>((resolve, reject) => {
      client.getSingBoxUserTraffic({}, validMetadata, (err: any, response: any) => {
        try {
          assert.ifError(err);
          assert.strictEqual(response.success, true);
          assert.strictEqual((response.entries ?? []).length, 0);
          resolve();
        } catch (e) {
          reject(e);
        }
      });
    });
  });

  await t.test('GetSingBoxUserTraffic fails gracefully (not a thrown error) when clash_api is configured but unreachable', async () => {
    // Порт 1 гарантированно никем не слушается — это ветка «блок есть, но sing-box не отвечает».
    // Отличать её от «блока нет» обязательно: первое — авария на ноде, второе — норма для ноды,
    // на которую ещё не приезжал конфиг.
    await fs.writeFile(
      tempConfigPath,
      JSON.stringify({ experimental: { clash_api: { external_controller: '127.0.0.1:1', secret: 'x' } } }),
      'utf-8'
    );

    await new Promise<void>((resolve, reject) => {
      client.getSingBoxUserTraffic({}, validMetadata, (err: any, response: any) => {
        try {
          assert.ifError(err, 'the RPC itself must always succeed at the transport level');
          assert.strictEqual(response.success, false);
          assert.strictEqual((response.entries ?? []).length, 0);
          resolve();
        } catch (e) {
          reject(e);
        }
      });
    });
  });

  await t.test('RunNetworkDiagnostic should block requests with invalid metadata tokens', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.runNetworkDiagnostic({ targets: ['example.com'] }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      // Empty repeated fields decode as `undefined` on the wire with this test client's
      // proto-loader config (no `defaults: true`), not `[]` -- tolerate both.
      assert.strictEqual((response.results ?? []).length, 0);
      done();
    });
  });

  await t.test('RunNetworkDiagnostic should reject an unsafe/flag-like target without crashing the whole call', (t, done) => {
    client.runNetworkDiagnostic({ targets: ['-oPonFire', 'also bad target'] }, validMetadata, (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true, 'overall success must be true even though every target was rejected');
        assert.strictEqual(response.results.length, 2);

        const first = response.results[0];
        assert.strictEqual(first.target, '-oPonFire');
        assert.strictEqual(first.reachable, false);
        assert.ok(first.error.includes('Invalid or unsafe target'));
        assert.strictEqual((first.lossyHops ?? first.lossy_hops ?? []).length, 0);

        const second = response.results[1];
        assert.strictEqual(second.target, 'also bad target');
        assert.strictEqual(second.reachable, false);
        assert.ok(second.error.includes('Invalid or unsafe target'));
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('RunNetworkDiagnostic should gracefully degrade to reachable:false when the mtr binary is missing (ENOENT)', (t, done) => {
    // mtr is not installed in this dev/test sandbox, and real packet-loss data is
    // inherently non-deterministic anyway -- ENOENT is the deterministic, environment-
    // independent negative path: it reproduces identically whether or not mtr happens
    // to be installed on the machine running this test.
    client.runNetworkDiagnostic({ targets: ['8.8.8.8'] }, validMetadata, (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true, 'a single unreachable/erroring target must not flip overall success to false');
        assert.strictEqual(response.results.length, 1);

        const result = response.results[0];
        assert.strictEqual(result.target, '8.8.8.8');
        assert.strictEqual(result.reachable, false);
        assert.ok(result.error, 'error message must be populated for the failed target');
        assert.strictEqual((result.lossyHops ?? result.lossy_hops ?? []).length, 0);
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('RunNetworkDiagnostic should isolate per-target failures: one bad target must not abort remaining targets', (t, done) => {
    client.runNetworkDiagnostic({ targets: ['-bad-flag-like-target', '1.1.1.1', 'valid-hostname.example'] }, validMetadata, (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.strictEqual(response.results.length, 3, 'all three targets must produce a result, in order, despite the first being rejected and the rest failing on missing mtr');

        assert.strictEqual(response.results[0].target, '-bad-flag-like-target');
        assert.strictEqual(response.results[0].reachable, false);
        assert.ok(response.results[0].error.includes('Invalid or unsafe target'));

        assert.strictEqual(response.results[1].target, '1.1.1.1');
        assert.strictEqual(response.results[1].reachable, false);
        assert.ok(response.results[1].error);

        assert.strictEqual(response.results[2].target, 'valid-hostname.example');
        assert.strictEqual(response.results[2].reachable, false);
        assert.ok(response.results[2].error);
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('RunNetworkDiagnostic should treat an empty targets list as a trivial success', (t, done) => {
    client.runNetworkDiagnostic({ targets: [] }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      assert.strictEqual((response.results ?? []).length, 0);
      done();
    });
  });

  await t.test('parseMtrJsonReport and isValidDiagnosticTarget pure-logic unit tests', async () => {
    const { parseMtrJsonReport, isValidDiagnosticTarget } = await import('../src/services/networkDiagnostic.service.js');

    // Reachable target: last hub resolves to a real host, no loss anywhere.
    const cleanReport = JSON.stringify({
      report: {
        hubs: [
          { count: 1, host: '192.168.1.1', 'Loss%': 0.0, Snt: 10, Avg: 0.5 },
          { count: 2, host: '8.8.8.8', 'Loss%': 0.0, Snt: 10, Avg: 12.3 }
        ]
      }
    });
    const cleanResult = parseMtrJsonReport(cleanReport);
    assert.strictEqual(cleanResult.reachable, true);
    assert.strictEqual(cleanResult.lossPercent, 0);
    assert.strictEqual(cleanResult.avgLatencyMs, 12.3);
    assert.deepStrictEqual(cleanResult.lossyHops, []);

    // Unreachable target: last hub is the "???" sentinel (no reply at all).
    const unreachableReport = JSON.stringify({
      report: {
        hubs: [
          { count: 1, host: '192.168.1.1', 'Loss%': 0.0, Snt: 10, Avg: 0.5 },
          { count: 2, host: '???', 'Loss%': 100.0, Snt: 10, Avg: 0.0 }
        ]
      }
    });
    const unreachableResult = parseMtrJsonReport(unreachableReport);
    assert.strictEqual(unreachableResult.reachable, false);
    assert.strictEqual(unreachableResult.lossPercent, 100);
    assert.strictEqual(unreachableResult.lossyHops.length, 1);
    assert.strictEqual(unreachableResult.lossyHops[0].hopNumber, 2);
    assert.strictEqual(unreachableResult.lossyHops[0].address, '???');
    assert.strictEqual(unreachableResult.lossyHops[0].lossPercent, 100);

    // Lossy intermediate hop but destination itself is reachable: only the lossy hop
    // must be included in lossyHops, not every hop (deliberate payload-size decision).
    const partialLossReport = JSON.stringify({
      report: {
        hubs: [
          { count: 1, host: '192.168.1.1', 'Loss%': 0.0, Snt: 10, Avg: 0.5 },
          { count: 2, host: 'flaky-router.example', 'Loss%': 20.0, Snt: 10, Avg: 5.2 },
          { count: 3, host: '1.1.1.1', 'Loss%': 0.0, Snt: 10, Avg: 8.1 }
        ]
      }
    });
    const partialLossResult = parseMtrJsonReport(partialLossReport);
    assert.strictEqual(partialLossResult.reachable, true);
    assert.strictEqual(partialLossResult.lossPercent, 0);
    assert.strictEqual(partialLossResult.lossyHops.length, 1);
    assert.strictEqual(partialLossResult.lossyHops[0].hopNumber, 2);
    assert.strictEqual(partialLossResult.lossyHops[0].address, 'flaky-router.example');
    assert.strictEqual(partialLossResult.lossyHops[0].lossPercent, 20);
    assert.strictEqual(partialLossResult.lossyHops[0].avgLatencyMs, 5.2);

    // Malformed / missing hubs must throw (caught by the handler's own try/catch, not here).
    assert.throws(() => parseMtrJsonReport('not json'), /Failed to parse mtr JSON output/);
    assert.throws(() => parseMtrJsonReport(JSON.stringify({ report: {} })), /did not contain any hubs/);
    assert.throws(() => parseMtrJsonReport(JSON.stringify({ report: { hubs: [] } })), /did not contain any hubs/);

    // Target validation: plausible hostnames/IPs accepted, flag-like/unsafe strings rejected.
    assert.strictEqual(isValidDiagnosticTarget('8.8.8.8'), true);
    assert.strictEqual(isValidDiagnosticTarget('example.com'), true);
    assert.strictEqual(isValidDiagnosticTarget('2001:db8::1'), true);
    assert.strictEqual(isValidDiagnosticTarget('sub.domain-name.example.com'), true);
    assert.strictEqual(isValidDiagnosticTarget('-oSomeFlag'), false, 'leading dash must be rejected (mtr would parse it as a flag)');
    assert.strictEqual(isValidDiagnosticTarget('--report-cycles'), false);
    assert.strictEqual(isValidDiagnosticTarget(''), false);
    assert.strictEqual(isValidDiagnosticTarget('   '), false);
    assert.strictEqual(isValidDiagnosticTarget('has a space'), false);
    assert.strictEqual(isValidDiagnosticTarget('rm;-rf'), false);
    assert.strictEqual(isValidDiagnosticTarget('$(whoami)'), false);
    assert.strictEqual(isValidDiagnosticTarget(null as any), false);
    assert.strictEqual(isValidDiagnosticTarget(undefined as any), false);
    assert.strictEqual(isValidDiagnosticTarget(123 as any), false);
    assert.strictEqual(isValidDiagnosticTarget('a'.repeat(254)), false, 'over-length hostnames must be rejected');
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

  // getWebRtcStatus (utils/telemetry.ts) was rewritten alongside the olcrtc-manager removal — it
  // no longer polls an HTTP API (that whole mechanism is gone), it checks the olcrtc-agent-srv
  // binary's presence on disk and, when present, counts running olcrtc-agent-srv@* systemd units
  // via `systemctl list-units`. Only the binary-presence branch (not_installed) is meaningfully
  // testable cross-platform here — `systemctl` doesn't exist on a non-Linux dev/CI box, so the
  // running-count branching (nominal/no_active_tunnels) isn't exercised by this suite, same as
  // other systemctl-touching code paths elsewhere in this file that only run for real under
  // `NODE_ENV !== 'test'` on an actual Linux node.
  await t.test('WebRTC status check logic (olcrtc-agent-srv binary presence)', async (t) => {
    process.env.TEST_WEBRTC_CHECK = 'true';

    t.after(async () => {
      delete process.env.TEST_WEBRTC_CHECK;
      await fs.unlink(tempOlcrtcAgentSrvPath).catch(() => {});
    });

    // Гарантируем отсутствие бинарника на диске (не полагаемся на порядок предыдущих тестов)
    await fs.unlink(tempOlcrtcAgentSrvPath).catch(() => {});

    await t.test('Should return not_installed if olcrtc-agent-srv binary is not present on disk', (t, done) => {
      const stream = client.streamTelemetry({ orchestratorSecret: 'test-secret-123' });
      stream.on('data', (data: any) => {
        try {
          assert.strictEqual(data.webrtcStatus, 'not_installed');
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
      const mtlsClient = new EgressAgentService('127.0.0.1:8083', sslCreds, {
        'grpc.ssl_target_name_override': 'localhost',
        'grpc.default_authority': 'localhost',
      });

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
      const badMtlsClient = new EgressAgentService('127.0.0.1:8083', badSslCreds, {
        'grpc.ssl_target_name_override': 'localhost',
        'grpc.default_authority': 'localhost',
      });

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

    await t.test('CADDY_RELOAD_COMMAND schema default includes the restart fallback, not just reload', async () => {
      // Regression test: `reload` re-POSTs config to the ALREADY-RUNNING Caddy process via its
      // admin API — it never re-reads EnvironmentFile=, so a process started before
      // /etc/caddy/cloudflare.env existed/had content stays permanently blind to CF_API_TOKEN no
      // matter how many times it's reloaded. Only a full restart re-execs and re-reads env fresh.
      // z.string().default() only fires when the env var is entirely ABSENT from process.env, so
      // the fallback MUST live in this schema default itself — a `config.CADDY_RELOAD_COMMAND ||
      // '...with restart...'` fallback elsewhere (as config.service.ts used to have) is
      // unreachable dead code, since the schema default already makes the value unconditionally
      // truthy. Caught live in production: 5 consecutive reload failures on a Xeon-ring node never
      // once triggered a restart, confirmed via the unchanged Caddy MainPID across every attempt.
      const { configSchema } = await import('../src/config.js');
      const parsed = configSchema.parse({ EGRESS_CONTROL_SECRET: 'test-secret-123' });
      assert.strictEqual(parsed.CADDY_RELOAD_COMMAND, 'systemctl reload caddy || systemctl restart caddy');
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

    await t.test('fixXraySocketPermissions should chmod 666 only sockets that exist, and never throw', async () => {
      const { fixXraySocketPermissions } = await import('../src/utils/caddy.js');

      const existingSocketPath = path.join(tempDir, 'fake_vless-xhttp.sock');
      const missingSocketPath = path.join(tempDir, 'fake_vless-grpc.sock');
      await fs.writeFile(existingSocketPath, '');
      await fs.rm(missingSocketPath, { force: true }).catch(() => {});

      // 1. Существующий сокет -> ровно один chmod 666 на него; отсутствующий -> exec не вызывается.
      const calls: string[] = [];
      const spyExec = async (command: string) => {
        calls.push(command);
        return { stdout: '', stderr: '' };
      };

      await fixXraySocketPermissions([existingSocketPath, missingSocketPath], spyExec);

      assert.strictEqual(calls.length, 1, 'expected exactly one chmod call, only for the existing socket');
      assert.strictEqual(calls[0], `chmod 666 ${existingSocketPath}`);

      // 2. Ни один путь не существует -> exec вообще не должен вызываться, ошибок нет.
      const missingCalls: string[] = [];
      const spyExecAllMissing = async (command: string) => {
        missingCalls.push(command);
        return { stdout: '', stderr: '' };
      };
      await fixXraySocketPermissions([missingSocketPath], spyExecAllMissing);
      assert.strictEqual(missingCalls.length, 0);

      // 3. exec бросает на первом пути -> функция не бросает, продолжает и пытается второй путь.
      const secondExistingSocketPath = path.join(tempDir, 'fake_vless-grpc_2.sock');
      await fs.writeFile(secondExistingSocketPath, '');

      const failingCalls: string[] = [];
      const spyExecFailingFirst = async (command: string) => {
        failingCalls.push(command);
        if (command.includes(existingSocketPath)) {
          throw new Error('simulated chmod failure');
        }
        return { stdout: '', stderr: '' };
      };

      await assert.doesNotReject(
        fixXraySocketPermissions([existingSocketPath, secondExistingSocketPath], spyExecFailingFirst)
      );
      assert.strictEqual(failingCalls.length, 2, 'expected the remaining path to still be attempted after a failure');
      assert.strictEqual(failingCalls[1], `chmod 666 ${secondExistingSocketPath}`);
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

  // Здесь стоял блок тестов singboxStats.parseUserTrafficStats — разбор формата имён статистики
  // v2ray_api (`user>>>{uuid}>>>traffic>>>{direction}`). Снят 2026-09-01 вместе с самой функцией:
  // канал переведён на Clash API, где никакого формата имён нет — есть готовые Upload/Download на
  // соединение. Содержательная логика теперь другая (подсчёт приращения между проходами) и живёт
  // в отдельном файле tests/singboxStats.test.ts — этому набору она не нужна, она чистая.
});


