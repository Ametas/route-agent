// tests/app.test.ts

import test from 'node:test';
import assert from 'node:assert';
import { promises as fs } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const tempConfigDir = path.join(__dirname, 'temp');
const tempConfigPath = path.join(tempConfigDir, 'sing-box-config.json');

// Конфигурируем тестовое окружение до загрузки модулей
process.env.NODE_ENV = 'test';
process.env.PORT = '8082';
process.env.HOST = '127.0.0.1';
process.env.EGRESS_CONTROL_SECRET = 'test-secret-123';
process.env.SINGBOX_CONFIG_PATH = tempConfigPath;
process.env.RELOAD_COMMAND = 'echo "mock reload"';

// Импортируем наш скомпилированный gRPC сервер для инициализации биндинга
const { startServer } = await import('../src/index.js');

const PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');
const packageDefinition = protoLoader.loadSync(PROTO_PATH, { keepCase: false });
const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
const EgressAgentService = protoDescriptor.agent.EgressAgentService;

test('Route Agent gRPC Pipeline Testing', async (t) => {
  await fs.rm(tempConfigDir, { recursive: true, force: true }).catch(() => {});
  await fs.mkdir(tempConfigDir, { recursive: true });

  const server = await startServer();

  // Создаем нативный клиент для тестов
  const client = new EgressAgentService(
    '127.0.0.1:8082',
    grpc.credentials.createInsecure()
  );

  t.after(async () => {
    client.close();
    server.forceShutdown();
    await fs.rm(tempConfigDir, { recursive: true, force: true }).catch(() => {});
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

    // Передаем валидный JSON объект конфигурации
    const payload = { log: { level: 'info' } };

    client.applyConfig({ configJson: JSON.stringify(payload) }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, true);
      done();
    });
  });

  await t.test('StreamTelemetry should stream telemetry containing webrtcStatus', (t, done) => {
    const stream = client.streamTelemetry({ orchestratorSecret: 'test-secret-123' });
    
    stream.on('data', (data: any) => {
      try {
        assert.ok(data.hasOwnProperty('webrtcStatus'));
        assert.strictEqual(data.webrtcStatus, 'nominal');
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
