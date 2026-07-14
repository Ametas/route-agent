import test from 'node:test';
import assert from 'node:assert';
import { promises as fs } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const tempConfigDir = path.join(__dirname, 'temp');
const tempConfigPath = path.join(tempConfigDir, 'sing-box-config.json');

// Set env vars before importing config/app
process.env.NODE_ENV = 'test';
process.env.PORT = '8082';
process.env.HOST = '127.0.0.1';
process.env.EGRESS_CONTROL_SECRET = 'test-secret-123';
process.env.SINGBOX_CONFIG_PATH = tempConfigPath;
process.env.RELOAD_COMMAND = process.platform === 'win32' ? 'echo reload' : 'echo reload';

const { server } = await import('../src/index.js');

test('Route Agent API', async (t) => {
  // Clean up any existing temp files before starting
  await fs.rm(tempConfigDir, { recursive: true, force: true }).catch(() => {});

  t.after(async () => {
    await fs.rm(tempConfigDir, { recursive: true, force: true }).catch(() => {});
  });

  await t.test('GET /agent/ping returns 200', async () => {
    const response = await server.inject({
      method: 'GET',
      url: '/agent/ping',
    });
    assert.strictEqual(response.statusCode, 200);
    const body = JSON.parse(response.body);
    assert.strictEqual(body.status, 'online');
    assert.ok(body.timestamp);
  });

  await t.test('POST /agent/config fails with 401 when header is missing', async () => {
    const response = await server.inject({
      method: 'POST',
      url: '/agent/config',
      payload: { dns: {} },
    });
    assert.strictEqual(response.statusCode, 401);
    const body = JSON.parse(response.body);
    assert.strictEqual(body.success, false);
    assert.strictEqual(body.error, 'Unauthorized');
    assert.strictEqual(body.message, 'Invalid orchestrator secret token.');
  });

  await t.test('POST /agent/config fails with 401 when header is incorrect', async () => {
    const response = await server.inject({
      method: 'POST',
      url: '/agent/config',
      headers: {
        'x-orchestrator-secret': 'wrong-secret',
      },
      payload: { dns: {} },
    });
    assert.strictEqual(response.statusCode, 401);
  });

  await t.test('POST /agent/config fails with 400 when body is not a JSON object', async () => {
    const response = await server.inject({
      method: 'POST',
      url: '/agent/config',
      headers: {
        'x-orchestrator-secret': 'test-secret-123',
        'content-type': 'application/json',
      },
      payload: [1, 2, 3],
    });
    assert.strictEqual(response.statusCode, 400);
    const body = JSON.parse(response.body);
    assert.strictEqual(body.success, false);
    assert.strictEqual(body.error, 'BadRequest');
  });

  await t.test('POST /agent/config succeeds with 200, writes file and runs reload command', async () => {
    const testConfigObj = {
      log: { level: 'info' },
      dns: { servers: ['8.8.8.8'] },
    };

    const response = await server.inject({
      method: 'POST',
      url: '/agent/config',
      headers: {
        'x-orchestrator-secret': 'test-secret-123',
      },
      payload: testConfigObj,
    });

    assert.strictEqual(response.statusCode, 200);
    const body = JSON.parse(response.body);
    assert.strictEqual(body.success, true);
    assert.strictEqual(body.message, 'Configuration successfully updated and sing-box reloaded.');

    // Verify config file was written correctly
    const fileExists = await fs.access(tempConfigPath).then(() => true).catch(() => false);
    assert.ok(fileExists, 'Config file should exist');

    const fileContentStr = await fs.readFile(tempConfigPath, 'utf8');
    const fileContent = JSON.parse(fileContentStr);
    assert.deepStrictEqual(fileContent, testConfigObj);
  });
});
