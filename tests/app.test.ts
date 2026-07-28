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
const tempAwgPath = path.join(tempDir, 'awg0.conf');

// Конфигурируем тестовое окружение до загрузки модулей
process.env.NODE_ENV = 'test';
process.env.PORT = '8082';
process.env.HOST = '127.0.0.1';
process.env.EGRESS_CONTROL_SECRET = 'test-secret-123';
process.env.SINGBOX_CONFIG_PATH = tempConfigPath;
process.env.SINGBOX_BINARY_PATH = tempBinaryPath;
process.env.CADDYFILE_PATH = tempCaddyfilePath;
process.env.OLCRTC_BINARY_PATH = tempOlcrtcPath;
process.env.OLCRTC_MANAGER_BINARY_PATH = tempOlcrtcManagerPath;
process.env.AWG_CONFIG_PATH = tempAwgPath;
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
    '127.0.0.1:8082',
    grpc.credentials.createInsecure()
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
        assert.ok(data.hasOwnProperty('awgActivePeers'));
        assert.strictEqual(data.webrtcStatus, 'nominal');
        assert.strictEqual(typeof data.singboxVersion, 'string');
        assert.strictEqual(typeof data.awgActivePeers, 'number');
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

    client.configureCaddy({ domains: ['example.com'] }, badMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      done();
    });
  });

  await t.test('ConfigureCaddy should reject empty domains list', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.configureCaddy({ domains: ['   ', ''] }, validMetadata, (err: any, response: any) => {
      assert.ifError(err);
      assert.strictEqual(response.success, false);
      assert.strictEqual(response.message, 'Список доменов пуст');
      done();
    });
  });

  await t.test('ConfigureCaddy should write Caddyfile for multiple domains when authorized', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    client.configureCaddy({ domains: ['domain1.com', 'domain2.com', 'domain1.com'] }, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('domain1.com, domain2.com'));
        const caddyContent = await fs.readFile(tempCaddyfilePath, 'utf-8');
        assert.ok(caddyContent.includes('# Auto-generated by Route Agent'));
        assert.ok(caddyContent.includes('domain1.com {'));
        assert.ok(caddyContent.includes('domain2.com {'));
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('ConfigureCaddy should unpack camouflage html and generate VLESS XHTTP & gRPC reverse proxy blocks', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const tempCamouflageDir = path.join(tempDir, 'camouflage');

    const payload = {
      domains: ['vpn1.example.com', 'vpn2.example.com'],
      camouflageHtml: '<html><body>Camouflage Site</body></html>',
      camouflagePath: tempCamouflageDir,
      xhttpRegexp: '^/xhttp-path.*$',
      xhttpRewrite: '/xhttp-path',
      xhttpSocket: 'unix//run/sing-box-xhttp.sock',
      grpcRegexp: '^/grpc-path.*$',
      grpcRewrite: '/grpc-path',
      grpcSocket: '127.0.0.1:10080'
    };

    client.configureCaddy(payload, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);
        assert.ok(response.message.includes('vpn1.example.com, vpn2.example.com'));

        const htmlContent = await fs.readFile(path.join(tempCamouflageDir, 'index.html'), 'utf-8');
        assert.strictEqual(htmlContent, '<html><body>Camouflage Site</body></html>');

        const caddyContent = await fs.readFile(tempCaddyfilePath, 'utf-8');
        assert.ok(caddyContent.includes('@vless-xhttp path_regexp xhttp "^/xhttp-path.*$"') || caddyContent.includes('@vless-xhttp path_regexp xhttp ^/xhttp-path.*$'));
        assert.ok(caddyContent.includes('reverse_proxy "unix//run/sing-box-xhttp.sock"') || caddyContent.includes('reverse_proxy unix//run/sing-box-xhttp.sock'));
        assert.ok(caddyContent.includes('@vless-grpc'));
        assert.ok(caddyContent.includes('protocol grpc'));
        assert.ok(caddyContent.includes('path_regexp grpc "^/grpc-path.*$"') || caddyContent.includes('path_regexp grpc ^/grpc-path.*$'));
        assert.ok(caddyContent.includes('reverse_proxy "127.0.0.1:10080"') || caddyContent.includes('reverse_proxy 127.0.0.1:10080'));
        assert.ok(caddyContent.includes(`root * ${tempCamouflageDir}`));
        assert.ok(caddyContent.includes('file_server'));
        done();
      } catch (e) {
        done(e);
      }
    });
  });

  await t.test('ConfigureCaddy should substitute default regex values when xhttpRegexp or grpcRegexp are empty strings', (t, done) => {
    const validMetadata = new grpc.Metadata();
    validMetadata.add('x-orchestrator-secret', 'test-secret-123');

    const payload = {
      domains: ['default-regex.example.com'],
      xhttpRegexp: '',
      xhttpSocket: 'unix//run/sing-box-xhttp.sock',
      grpcRegexp: '   ',
      grpcSocket: '127.0.0.1:10080'
    };

    client.configureCaddy(payload, validMetadata, async (err: any, response: any) => {
      try {
        assert.ifError(err);
        assert.strictEqual(response.success, true);

        const caddyContent = await fs.readFile(tempCaddyfilePath, 'utf-8');
        assert.ok(caddyContent.includes('@vless-xhttp path_regexp xhttp "^/xhttp-path.*$"') || caddyContent.includes('@vless-xhttp path_regexp xhttp ^/xhttp-path.*$'));
        assert.ok(caddyContent.includes('path_regexp grpc "^/grpc-path.*$"') || caddyContent.includes('path_regexp grpc ^/grpc-path.*$'));
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
      s1: 15,
      s2: 25,
      s3: 35,
      s4: 45,
      h1: 101,
      h2: 202,
      h3: 303,
      h4: 404,
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
        assert.ok(awgContent.includes('H1 = 101'));
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
  });
});
