import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { configureCaddyHandler } from '../src/services/config.service.js';
import { getCaddyCertPaths } from '../src/utils/certStorage.js';

process.env.NODE_ENV = 'test';

/**
 * Повторный пуш конфига Caddy с тем же содержимым не должен ничего делать.
 *
 * ЖИВОЙ СЛУЧАЙ (2026-09-04, по логам владельца): два одинаковых `ConfigureCaddy` в одну секунду —
 * шаг бота зовёт синк явно, а `pushConfigToNode` делает его же пресинком внутри себя. Второй проход
 * перезаписывал те же файлы, порождал процесс `caddy validate` и ПЕРЕЗАГРУЖАЛ Caddy на живом узле.
 * Сертификаты при этом заново не выпускались — они уже были, — но перезагрузка фронтового Caddy
 * бесплатной не бывает.
 *
 * Лечится на стороне узла, а не в вызывающем: повторный синк обязан быть дешёвым, кто бы его ни
 * инициировал.
 */

interface CaddyResponse {
  success: boolean;
  message: string;
}

const CADDYFILE = 'example.com {\n    respond "ok"\n}\n';

async function callHandler(request: Record<string, unknown>): Promise<CaddyResponse> {
  return new Promise((resolve, reject) => {
    const call = {
      metadata: { get: () => [config.EGRESS_CONTROL_SECRET] },
      request,
    };
    void configureCaddyHandler(call as never, ((err: unknown, res: CaddyResponse) => {
      if (err) reject(err);
      else resolve(res);
    }) as never);
  });
}

/**
 * Сертификаты пишутся в НАСТОЯЩИЙ каталог Caddy: путь к нему в обработчике зашит и параметром не
 * задаётся. Убираем за собой сами, чтобы прогон тестов не оставлял мусора вне временного каталога.
 */
async function removeCertDir(domain: string): Promise<void> {
  const { certPath } = getCaddyCertPaths(domain);
  await fs.rm(path.dirname(certPath), { recursive: true, force: true }).catch(() => {});
}

async function withTempCaddy<T>(body: (dir: string) => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'caddy-cfg-'));
  const original = config.CADDYFILE_PATH;
  config.CADDYFILE_PATH = path.join(dir, 'Caddyfile');
  try {
    return await body(dir);
  } finally {
    config.CADDYFILE_PATH = original;
    delete process.env.CADDY_TEST_INACTIVE;
    await removeCertDir('ring.example.com');
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('an identical Caddy config is neither rewritten nor reloaded', async () => {
  await withTempCaddy(async () => {
    const first = await callHandler({ caddyfileContent: CADDYFILE });
    assert.strictEqual(first.success, true, first.message);

    const writtenAt = (await fs.stat(config.CADDYFILE_PATH)).mtimeMs;
    await new Promise((r) => setTimeout(r, 20));

    const second = await callHandler({ caddyfileContent: CADDYFILE });

    assert.strictEqual(second.success, true, second.message);
    // Судим по времени изменения файла, а не по тексту ответа: текст — это то, что агент про себя
    // рассказывает, а mtime — то, что он на самом деле сделал.
    assert.strictEqual((await fs.stat(config.CADDYFILE_PATH)).mtimeMs, writtenAt, 'конфиг переписан без изменений');
    assert.match(second.message, /already current/);
  });
});

test('a changed Caddyfile is written', async () => {
  await withTempCaddy(async () => {
    await callHandler({ caddyfileContent: CADDYFILE });

    const changed = CADDYFILE.replace('ok', 'changed');
    const result = await callHandler({ caddyfileContent: changed });

    assert.strictEqual(result.success, true, result.message);
    assert.strictEqual(await fs.readFile(config.CADDYFILE_PATH, 'utf-8'), changed);
    assert.doesNotMatch(result.message, /already current/);
  });
});

test('a changed certificate is written even when the Caddyfile itself is identical', async () => {
  /**
   * ЛОВУШКА, ради которой сравнивается ВЕСЬ набор файлов, а не один Caddyfile. Кольцевые
   * сертификаты меняются независимо от него: фронт перевыпустил wildcard — Caddyfile тот же,
   * а материал другой. Сравнить только главный файл значило бы заменить лишнюю перезагрузку тихой
   * недоставкой сертификата, то есть починить мелочь ценой отказа.
   */
  await withTempCaddy(async () => {
    const certOf = (pem: string) => ({
      caddyfileContent: CADDYFILE,
      extraCertificates: [{ domain: 'ring.example.com', certPem: pem, keyPem: 'KEY-MATERIAL\n' }],
    });

    await callHandler(certOf('CERT-V1\n'));
    const result = await callHandler(certOf('CERT-V2\n'));

    assert.doesNotMatch(result.message, /already current/, 'обновлённый сертификат сочли за «ничего не изменилось»');
  });
});

test('an identical config still reloads when Caddy is down', async () => {
  /**
   * Пропуск завязан на ДВА условия, и второе легко счесть лишним. Без него узел с верным конфигом,
   * но упавшей службой остался бы лежать: оркестратор возит ему тот же конфиг, агент каждый раз
   * отвечает «уже актуально», и поднять Caddy некому.
   */
  await withTempCaddy(async () => {
    await callHandler({ caddyfileContent: CADDYFILE });
    const writtenAt = (await fs.stat(config.CADDYFILE_PATH)).mtimeMs;

    process.env.CADDY_TEST_INACTIVE = '1';
    await new Promise((r) => setTimeout(r, 20));
    const result = await callHandler({ caddyfileContent: CADDYFILE });

    assert.doesNotMatch(result.message, /already current/, 'лежащий Caddy сочли за «уже актуально»');
    assert.notStrictEqual((await fs.stat(config.CADDYFILE_PATH)).mtimeMs, writtenAt);
  });
});

test('a private key whose mode drifted is rewritten, not skipped', async () => {
  /**
   * Содержимое совпадает, а права разъехались — это приватный ключ, открытый лишним читателям.
   * Считать такой файл актуальным значило бы законсервировать проблему: пропуск записи пропустил бы
   * и `chmod`.
   *
   * На Windows права POSIX не воспроизводятся, поэтому проверка идёт только там, где они реальны.
   */
  if (process.platform === 'win32') return;

  await withTempCaddy(async () => {
    const payload = {
      caddyfileContent: CADDYFILE,
      extraCertificates: [{ domain: 'ring.example.com', certPem: 'CERT\n', keyPem: 'KEY\n' }],
    };
    await callHandler(payload);

    const keyPath = path.join(
      '/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory',
      'ring.example.com',
      'ring.example.com.key'
    );
    const keyExists = await fs.stat(keyPath).then(() => true).catch(() => false);
    if (!keyExists) return; // каталог Caddy недоступен в этой среде — проверять нечего

    await fs.chmod(keyPath, 0o644);
    const result = await callHandler(payload);

    assert.doesNotMatch(result.message, /already current/, 'разъехавшиеся права сочли за «уже актуально»');
    assert.strictEqual((await fs.stat(keyPath)).mode & 0o777, 0o600, 'права ключа не восстановлены');
  });
});
