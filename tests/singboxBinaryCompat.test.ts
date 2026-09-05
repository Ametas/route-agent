import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { verifyBinaryAcceptsLiveConfigs } from '../src/services/binary.service.js';

process.env.NODE_ENV = 'test';

/**
 * Новый бинарь sing-box обязан доказать, что принимает УЖЕ ЛЕЖАЩИЕ на узле конфиги, ДО того как
 * его поставят на место работающего.
 *
 * Откуда взялась потребность. Конфиг проверяется `sing-box check` при каждом применении, но
 * проверяет его ТЕКУЩИЙ бинарь. Смена бинаря меняет судью: сборки различаются набором фич и
 * строгостью разбора. Наш форк, например, отвергает набор абонентов с негодным UUID целиком, тогда
 * как апстрим молча подставляет `uuid.NewV5` от строки. Раньше последовательность была: подменили
 * бинарь -> `systemctl restart` -> не стартует -> узел без сервиса, и откатываться НЕ НА ЧТО,
 * старый бинарь уже перезаписан.
 */

async function withConfigs<T>(
  files: Array<{ name: string; content: string }>,
  body: (dir: string) => Promise<T>
): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'sb-compat-'));
  for (const file of files) {
    await fs.writeFile(path.join(dir, file.name), file.content, 'utf-8');
  }

  const originalMain = config.SINGBOX_CONFIG_PATH;
  const originalRear = config.REAR_SINGBOX_CONFIG_PATH;
  config.SINGBOX_CONFIG_PATH = path.join(dir, 'config.json');
  config.REAR_SINGBOX_CONFIG_PATH = path.join(dir, 'rear.json');

  try {
    return await body(dir);
  } finally {
    config.SINGBOX_CONFIG_PATH = originalMain;
    config.REAR_SINGBOX_CONFIG_PATH = originalRear;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

/**
 * Свежий узел: конфигов ещё нет. Требовать их наличия значило бы запретить первую установку
 * бинаря вовсе — поэтому отсутствующий конфиг пропускается, а не считается провалом.
 */
test('a node with no configs yet accepts any binary — nothing to contradict', async () => {
  const verdict = await withConfigs([], () => verifyBinaryAcceptsLiveConfigs('git'));
  assert.deepStrictEqual(verdict, { ok: true });
});

/**
 * Роль бинаря играет `git`: на `git check -c <path>` он гарантированно завершается ошибкой, то
 * есть ведёт себя ровно как sing-box, отвергнувший конфиг. Настоящая сборка тут не нужна —
 * проверяется решение агента, а не разбор конфига.
 */
test('a binary that rejects an already-applied config is refused, naming that config', async () => {
  const verdict = await withConfigs(
    [{ name: 'config.json', content: '{"log":{"level":"warn"}}' }],
    () => verifyBinaryAcceptsLiveConfigs('git')
  );

  assert.strictEqual(verdict.ok, false);
  if (verdict.ok) return;
  assert.match(verdict.configPath, /config\.json$/);
  assert.notStrictEqual(verdict.error, '');
});

/**
 * Бинарь общий у фронтового и тылового (WARP) инстансов, а конфиги у них разные — отвергнуть новая
 * сборка может именно тыловой, и промолчать об этом нельзя.
 */
test('the rear instance config is checked too, not just the front one', async () => {
  const verdict = await withConfigs(
    [{ name: 'rear.json', content: '{"log":{"level":"warn"}}' }],
    () => verifyBinaryAcceptsLiveConfigs('git')
  );

  assert.strictEqual(verdict.ok, false);
  if (verdict.ok) return;
  assert.match(verdict.configPath, /rear\.json$/);
});
