import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { allWritesAlreadyOnDisk, applyPlannedWrites } from '../src/utils/plannedWrites.js';

process.env.NODE_ENV = 'test';

/**
 * Сравнение «а не лежит ли уже ровно это» — сердцевина пропуска лишней перезагрузки Caddy.
 *
 * Проверяется отдельно от обработчика намеренно: через него две ветки недостижимы. Пустой план в
 * `ConfigureCaddy` невозможен (Caddyfile попадает туда всегда), а сравнение прав на Windows не
 * воспроизводится в том виде, в каком оно работает на узле.
 */

async function withTempDir<T>(body: (dir: string) => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'planned-writes-'));
  try {
    return await body(dir);
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('совпадением считается только полное совпадение содержимого', async () => {
  await withTempDir(async (dir) => {
    const target = path.join(dir, 'a.conf');
    const writes = [{ path: target, content: 'hello\n' }];

    assert.strictEqual(await allWritesAlreadyOnDisk(writes), false, 'отсутствующий файл сочли совпадением');

    await applyPlannedWrites(writes);
    assert.strictEqual(await allWritesAlreadyOnDisk(writes), true);

    assert.strictEqual(
      await allWritesAlreadyOnDisk([{ path: target, content: 'hello world\n' }]),
      false,
      'другое содержимое сочли совпадением'
    );
  });
});

test('расхождение хотя бы в одном файле — это расхождение', async () => {
  /**
   * Ровно та ловушка, ради которой сравнивается ВЕСЬ набор: Caddyfile тот же, а сертификат другой.
   * Ответить «уже актуально» здесь значило бы заменить лишнюю перезагрузку тихой недоставкой.
   */
  await withTempDir(async (dir) => {
    const writes = [
      { path: path.join(dir, 'Caddyfile'), content: 'site {}\n' },
      { path: path.join(dir, 'ring.crt'), content: 'CERT-V1\n' },
    ];
    await applyPlannedWrites(writes);

    const withNewCert = [writes[0], { path: writes[1].path, content: 'CERT-V2\n' }];

    assert.strictEqual(await allWritesAlreadyOnDisk(withNewCert), false);
  });
});

test('пустой план совпадением НЕ считается', async () => {
  /**
   * «Писать нечего» и «на узле всё верно» — разные вещи. Ответить совпадением на пустой план
   * значило бы отчитаться о состоянии, которого никто не проверял.
   */
  assert.strictEqual(await allWritesAlreadyOnDisk([]), false);
});

test('разъехавшиеся права — это расхождение, даже при верном содержимом', async () => {
  /**
   * Файл с верным содержимым, но открытыми правами — это приватный ключ, доступный лишним
   * читателям. Считать его актуальным значило бы законсервировать проблему: пропуск записи
   * пропустил бы и `chmod`.
   *
   * Проверяется от обратного, чтобы работать и там, где POSIX-права не воспроизводятся: ожидание
   * 0600 против фактических прав обычного файла заведомо не совпадёт.
   */
  await withTempDir(async (dir) => {
    const target = path.join(dir, 'secret.key');
    await fs.writeFile(target, 'KEY\n', 'utf-8');
    await fs.chmod(target, 0o644).catch(() => {});

    const strict = [{ path: target, content: 'KEY\n', mode: 0o600 }];

    assert.strictEqual(await allWritesAlreadyOnDisk(strict), false, 'права не участвуют в сравнении');
  });
});

test('запись выставляет права и на уже существующем файле', async () => {
  /**
   * `mode` у `writeFile` действует только при СОЗДАНИИ. У существующего файла права остались бы
   * прежними — то есть однажды открытый ключ так и остался бы открытым. Отсюда отдельный `chmod`.
   */
  if (process.platform === 'win32') return;

  await withTempDir(async (dir) => {
    const target = path.join(dir, 'secret.key');
    await fs.writeFile(target, 'OLD\n', 'utf-8');
    await fs.chmod(target, 0o644);

    await applyPlannedWrites([{ path: target, content: 'KEY\n', mode: 0o600 }]);

    assert.strictEqual((await fs.stat(target)).mode & 0o777, 0o600);
  });
});

test('создаёт недостающие каталоги', async () => {
  await withTempDir(async (dir) => {
    const target = path.join(dir, 'nested', 'deep', 'file.pem');

    await applyPlannedWrites([{ path: target, content: 'PEM\n' }]);

    assert.strictEqual(await fs.readFile(target, 'utf-8'), 'PEM\n');
  });
});
