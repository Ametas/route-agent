import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { configureRearSingboxHandler } from '../src/services/rearSingbox.service.js';

// Как и в остальных файлах тестов: без этого обработчик уходит в проверку наличия бинаря sing-box
// и отвечает `singbox_not_installed`, не дойдя до самой логики.
process.env.NODE_ENV = 'test';

/**
 * Пуш конфига тыла, который ничего не меняет, не должен ничего делать.
 *
 * ЗАЧЕМ ЭТО ВАЖНО. Перезагрузка тыла не бесплатна: по SIGHUP sing-box закрывает инстанс целиком и
 * соединения рвутся. Раньше агент писал и перезагружал ВСЕГДА, даже получив байт в байт тот же
 * конфиг. Из-за этой цены оркестратору приходилось экономить на доставках — а экономия обернулась
 * нодами, до которых свежие WARP-ключи не доезжали вовсе (живой случай 2026-09-03: десять ключей в
 * базе, один в конфиге). Дешёвая доставка снимает саму причину экономить.
 */

interface RearResponse {
  success: boolean;
  message: string;
  running: boolean;
}

const REAR_CONFIG = {
  log: { level: 'warn' },
  inbounds: [{ type: 'vless', tag: 'stars-in', listen: '127.0.0.1', listen_port: 29000 }],
  outbounds: [{ type: 'direct', tag: 'direct' }],
};

async function callHandler(configObj: object): Promise<RearResponse> {
  return new Promise((resolve, reject) => {
    const call = {
      metadata: { get: () => [config.EGRESS_CONTROL_SECRET] },
      request: { enabled: true, configJson: JSON.stringify(configObj) },
    };
    void configureRearSingboxHandler(call as never, ((err: unknown, res: RearResponse) => {
      if (err) reject(err);
      else resolve(res);
    }) as never);
  });
}

test('a byte-identical rear config is neither rewritten nor reloaded', async (t) => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-unchanged-'));
  const configPath = path.join(dir, 'rear.json');
  const unitPath = path.join(dir, 'route-rear-singbox.service');
  const originalConfigPath = config.REAR_SINGBOX_CONFIG_PATH;
  const originalUnitPath = config.REAR_SINGBOX_UNIT_FILE_PATH;
  config.REAR_SINGBOX_CONFIG_PATH = configPath;
  config.REAR_SINGBOX_UNIT_FILE_PATH = unitPath;

  t.after(async () => {
    config.REAR_SINGBOX_CONFIG_PATH = originalConfigPath;
    config.REAR_SINGBOX_UNIT_FILE_PATH = originalUnitPath;
    await fs.rm(dir, { recursive: true, force: true });
  });

  const first = await callHandler(REAR_CONFIG);
  assert.strictEqual(first.success, true, first.message);
  const writtenAt = (await fs.stat(configPath)).mtimeMs;

  /**
   * Второй пуш того же конфига. Судим по времени изменения файла, а не по тексту ответа: текст —
   * это то, что агент про себя рассказывает, а mtime — то, что он на самом деле сделал.
   */
  await new Promise((r) => setTimeout(r, 20));
  const second = await callHandler(REAR_CONFIG);

  assert.strictEqual(second.success, true, second.message);
  assert.strictEqual(second.running, true);
  assert.strictEqual((await fs.stat(configPath)).mtimeMs, writtenAt, 'конфиг переписан без изменений');
  assert.match(second.message, /already current/);
});

test('a changed rear config is written', async (t) => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-changed-'));
  const configPath = path.join(dir, 'rear.json');
  const unitPath = path.join(dir, 'route-rear-singbox.service');
  const originalConfigPath = config.REAR_SINGBOX_CONFIG_PATH;
  const originalUnitPath = config.REAR_SINGBOX_UNIT_FILE_PATH;
  config.REAR_SINGBOX_CONFIG_PATH = configPath;
  config.REAR_SINGBOX_UNIT_FILE_PATH = unitPath;

  t.after(async () => {
    config.REAR_SINGBOX_CONFIG_PATH = originalConfigPath;
    config.REAR_SINGBOX_UNIT_FILE_PATH = originalUnitPath;
    await fs.rm(dir, { recursive: true, force: true });
  });

  await callHandler(REAR_CONFIG);

  /**
   * Ровно тот случай, ради которого всё и делается: у ноды появился WARP-туннель. Пропустить такой
   * пуш означало бы оставить её без WARP при готовых ключах — то есть заменить одну поломку другой.
   */
  const withEndpoint = {
    ...REAR_CONFIG,
    endpoints: [{ type: 'wireguard', tag: 'warp-key-1', address: ['172.16.0.2/32'] }],
  };
  const result = await callHandler(withEndpoint);

  assert.strictEqual(result.success, true, result.message);
  const onDisk = JSON.parse(await fs.readFile(configPath, 'utf-8'));
  assert.ok(onDisk.endpoints, 'изменившийся конфиг не записан');
  assert.doesNotMatch(result.message, /already current/);
});

test('an identical config still restarts a rear instance that is down', async (t) => {
  /**
   * Пропуск завязан на ДВА условия, и второе легко счесть лишним: конфиг не изменился И тыл
   * работает. Без второго нода, у которой тыл лёг, осталась бы лежать — оркестратор возит ей тот же
   * самый конфиг, агент каждый раз отвечает «уже актуально», и поднять инстанс некому.
   *
   * Проверяется через `REAR_TEST_INACTIVE`, потому что `systemctl` в тестах закорочен: мутационный
   * прогон показал, что без этого шва выброшенная проверка живости не роняет ни одного теста.
   */
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-down-'));
  const configPath = path.join(dir, 'rear.json');
  const unitPath = path.join(dir, 'route-rear-singbox.service');
  const originalConfigPath = config.REAR_SINGBOX_CONFIG_PATH;
  const originalUnitPath = config.REAR_SINGBOX_UNIT_FILE_PATH;
  config.REAR_SINGBOX_CONFIG_PATH = configPath;
  config.REAR_SINGBOX_UNIT_FILE_PATH = unitPath;

  t.after(async () => {
    config.REAR_SINGBOX_CONFIG_PATH = originalConfigPath;
    config.REAR_SINGBOX_UNIT_FILE_PATH = originalUnitPath;
    delete process.env.REAR_TEST_INACTIVE;
    await fs.rm(dir, { recursive: true, force: true });
  });

  await callHandler(REAR_CONFIG);
  const writtenAt = (await fs.stat(configPath)).mtimeMs;

  process.env.REAR_TEST_INACTIVE = '1';
  await new Promise((r) => setTimeout(r, 20));
  const result = await callHandler(REAR_CONFIG);

  assert.doesNotMatch(result.message, /already current/, 'лежащий тыл сочли за «уже актуально»');
  assert.notStrictEqual((await fs.stat(configPath)).mtimeMs, writtenAt, 'конфиг не переписан, значит и запуска не было');
});
