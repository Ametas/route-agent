// tests/app.test.ts

import test from 'node:test';
import assert from 'node:assert';
import { promises as fs } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const tempDir = path.join(__dirname, 'temp');
const tempConfigPath = path.join(tempDir, 'sing-box-config.json');
const tempBinaryPath = path.join(tempDir, 'sing-box');
const tempCaddyfilePath = path.join(tempDir, 'Caddyfile');
const tempOlcrtcPath = path.join(tempDir, 'olcrtc');
const tempOlcrtcManagerPath = path.join(tempDir, 'olcrtc-manager');

// Конфигурируем тестовое окружение до загрузки модулей
process.env.NODE_ENV = 'test';
process.env.PORT = '8088';
process.env.HOST = '127.0.0.1';
process.env.EGRESS_CONTROL_SECRET = 'test-secret-123';
process.env.SINGBOX_CONFIG_PATH = tempConfigPath;
process.env.SINGBOX_BINARY_PATH = tempBinaryPath;
process.env.CADDYFILE_PATH = tempCaddyfilePath;
process.env.OLCRTC_BINARY_PATH = tempOlcrtcPath;
process.env.OLCRTC_MANAGER_BINARY_PATH = tempOlcrtcManagerPath;
process.env.RELOAD_COMMAND = 'echo "mock reload"';
process.env.CADDY_RELOAD_COMMAND = 'echo "mock caddy reload"';

// Импортируем наш скомпилированный gRPC сервер для инициализации биндинга
const { startServer } = await import('../src/index.js');

const PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');
const packageDefinition = protoLoader.loadSync(PROTO_PATH, { keepCase: false });
const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
const EgressAgentService = protoDescriptor.agent.EgressAgentService;

test('Route Agent gRPC Pipeline Testing', async (t) => {
  await fs.rm(tempDir, { recursive: true, force: true }).catch(() => {});
  await fs.mkdir(tempDir, { recursive: true });

  const server = await startServer();

  // Создаем нативный клиент для тестов
  const client = new EgressAgentService(
    '127.0.0.1:8088',
    grpc.credentials.createInsecure()
  );

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

    client.applyConfig({ configJson: JSON.stringify(payload) }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      done();
    });
  });

  await t.test('StreamTelemetry should stream telemetry containing webrtcStatus and singboxVersion', (t, done) => {
    const stream = client.streamTelemetry({ orchestratorSecret: 'test-secret-123' });
    
    stream.on('data', (data: any) => {
      try {
        assert.ok(data.hasOwnProperty('webrtcStatus'));
        assert.ok(data.hasOwnProperty('singboxVersion'));
        assert.strictEqual(data.webrtcStatus, 'nominal');
        assert.strictEqual(typeof data.singboxVersion, 'string');
        assert.strictEqual(typeof data.cpuUsage, 'number');
        assert.strictEqual(typeof data.memUsage, 'number');
        assert.strictEqual(typeof data.activeConnections, 'number');
        assert.strictEqual(typeof data.systemLogs, 'string');
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

  await t.test('UploadSingboxBinary should block stream with invalid secret', (t, done) => {
    const call = client.uploadSingboxBinary((err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Invalid orchestrator secret token.');
      done();
    });
    call.write({ orchestratorSecret: 'invalid_secret', chunk: Buffer.from('test data'), version: '1.12.0', isFinal: true });
    call.end();
  });

  await t.test('UploadSingboxBinary should upload binary chunks and update target binary file', (t, done) => {
    const call = client.uploadSingboxBinary(async (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      assert.ok(response.message.includes('1.12.0'));
      const exists = await fs.stat(tempBinaryPath).then(() => true).catch(() => false);
      assert.strictEqual(exists, true);
      done();
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('binary_chunk_1\n'), version: '1.12.0', isFinal: false });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('binary_chunk_2\n'), version: '1.12.0', isFinal: true });
    call.end();
  });

  await t.test('UploadOlcrtcBinary should upload olcrtc / olcrtc-manager binaries', (t, done) => {
    const call = client.uploadOlcrtcBinary(async (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      assert.ok(response.message.includes('olcrtc-manager'));
      const exists = await fs.stat(tempOlcrtcManagerPath).then(() => true).catch(() => false);
      assert.strictEqual(exists, true);
      done();
    });
    call.write({ orchestratorSecret: 'test-secret-123', chunk: Buffer.from('olcrtc_mgr_chunk'), version: '1.0.0', targetBinary: 'olcrtc-manager', isFinal: true });
    call.end();
  });

  await t.test('ConfigureCaddy should block unauthorized requests', (t, done) => {
    const badMetadata = new grpc.Metadata();
    badMetadata.add('x-orchestrator-secret', 'bad_secret');

    client.configureCaddy({ domain: 'example.com', decoyPort: 8443, htmlContent: '<h1>Hello</h1>' }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('ConfigureCaddy should write Caddyfile and HTML content when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.configureCaddy({ domain: 'decoy.example.com', decoyPort: 8443, htmlContent: '<h1>Decoy Site</h1>' }, validMetadata, async (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      const caddyContent = await fs.readFile(tempCaddyfilePath, 'utf-8');
      assert.ok(caddyContent.includes('decoy.example.com:8443'));
      done();
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
});
