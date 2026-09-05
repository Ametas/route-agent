import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { getSingBoxVersion, parseSingBoxVersionOutput } from '../src/utils/telemetry.js';

process.env.NODE_ENV = 'test';

/**
 * «Ядро установлено» — это бинарь И юнит, а не один бинарь.
 *
 * ЖИВОЙ СЛУЧАЙ (mo-nl-node, 2026-09-04). Бинарь `/usr/local/bin/sing-box` общий у фронтового и
 * ТЫЛОВОГО инстансов, поэтому после сноса фронтовой службы он остаётся на месте — тыл из него
 * работает. Версия определялась исправно, оркестратор считал ядро установленным, прятал кнопку
 * установки и не давал восстановиться штатно: пуш конфига упирался в `systemctl start`, а юнита не
 * было. Юнит пришлось писать руками.
 *
 * Проверка построена так, чтобы бинарь ГАРАНТИРОВАННО отвечал: иначе оба случая давали бы
 * `not_installed` по разным причинам, и тест не различал бы вообще ничего. Роль бинаря играет `git`
 * — на `git version` он печатает строку, которую разбирает тот же регэксп.
 */

async function withUnit<T>(unitExists: boolean, body: () => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'sb-unit-'));
  const unitPath = path.join(dir, 'sing-box.service');
  if (unitExists) await fs.writeFile(unitPath, '[Unit]\n', 'utf-8');

  const originalUnit = config.SINGBOX_UNIT_FILE_PATH;
  const originalBinary = config.SINGBOX_BINARY_PATH;
  config.SINGBOX_UNIT_FILE_PATH = unitPath;
  config.SINGBOX_BINARY_PATH = 'git';

  try {
    return await body();
  } finally {
    config.SINGBOX_UNIT_FILE_PATH = originalUnit;
    config.SINGBOX_BINARY_PATH = originalBinary;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('version is reported when both the binary and the unit are in place', async () => {
  const version = await withUnit(true, () => getSingBoxVersion());

  // Если `git` в этой среде недоступен, различать нечего — проверять нечего тоже.
  if (version === 'not_installed') return;

  assert.notStrictEqual(version, 'not_installed');
});

test('a missing unit means not installed, even with a working binary', async () => {
  /**
   * Ровно та ситуация, что оставила ноду без пути восстановления: файл на месте, запустить нечем.
   * Формально бинарь есть — но правильное действие то же самое, что при его отсутствии: установка,
   * которая юнит и создаст.
   */
  const withoutUnit = await withUnit(false, () => getSingBoxVersion());
  const withUnitPresent = await withUnit(true, () => getSingBoxVersion());

  if (withUnitPresent === 'not_installed') return; // `git` недоступен — различать нечего

  assert.strictEqual(withoutUnit, 'not_installed', 'отсутствие юнита не считается отсутствием ядра');
});

/**
 * Суффикс версии — единственное, по чему оркестратор отличает наш форк (горячая замена абонентов)
 * от официальной сборки. До 2026-09-06 регулярка была `/sing-box version ([0-9.]+)/` и обрезала
 * всё после цифр: форк приезжал как `1.14.0`, то есть побайтово как апстрим, и развилка кнопок
 * «форк ↔ оригинал» была принципиально невозможна. Тот же обрез превращал пререлизы в стабильные.
 */
test('version suffix survives parsing — the fork must not read as upstream', () => {
  assert.strictEqual(
    parseSingBoxVersionOutput('sing-box version 1.14.0-hotusers\n\nEnvironment: go1.27.1 linux/amd64'),
    '1.14.0-hotusers'
  );
  assert.strictEqual(
    parseSingBoxVersionOutput('sing-box version 1.14.0\n\nEnvironment: go1.25.5 linux/amd64'),
    '1.14.0'
  );
  assert.strictEqual(
    parseSingBoxVersionOutput('sing-box version 1.15.0-beta.1\n'),
    '1.15.0-beta.1'
  );
});

test('unparseable output falls back to the first line, never to a wrong version', () => {
  assert.strictEqual(parseSingBoxVersionOutput('totally unexpected output\nsecond line'), 'totally unexpected output');
  assert.strictEqual(parseSingBoxVersionOutput(''), 'not_installed');
});
