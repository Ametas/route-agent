import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { parseClashConnections, readClashApiEndpoint } from '../src/utils/singboxConnections.js';

/**
 * Разбор записей о соединениях из локального Clash API sing-box'а (2026-08-30).
 *
 * Канал заменил прежний `v2ray_api`, которого нет в официальных сборках sing-box — конфиг с тем
 * блоком не проходил даже `sing-box check`. Здесь проверяется только разбор: сетевой вызов отделён
 * от него намеренно, чтобы логику можно было прогнать без живого sing-box'а.
 *
 * Что важно удержать:
 *  - направления НЕ складываются: на различии «отдал» против «принял» стоит вся проверка на флуд;
 *  - форма ответа приходит извне, поэтому кривое поле роняет одну запись в ноль, а не весь ответ;
 *  - адрес и секрет читаются из ПРИМЕНЁННОГО конфига, а не из настроек агента — иначе они могли бы
 *    разойтись с тем, что реально работает на ноде.
 */

function clashResponse(connections: unknown[]): unknown {
  return { connections };
}

describe('разбор соединений Clash API', () => {
  it('раскладывает запись по полям, не смешивая направления', () => {
    const [record] = parseClashConnections(
      clashResponse([
        {
          upload: 1200,
          download: 340,
          start: '2026-08-30T10:00:00.000Z',
          metadata: {
            network: 'tcp',
            sourceIP: '203.0.113.9',
            destinationIP: '198.51.100.4',
            destinationPort: '443',
            host: 'example.com',
            inboundTag: 'tuic-in-own',
            user: 'user-uuid-1',
          },
        },
      ]),
      false
    );

    assert.equal(record.user, 'user-uuid-1');
    assert.equal(record.sourceIp, '203.0.113.9');
    assert.equal(record.destinationIp, '198.51.100.4');
    assert.equal(record.destinationDomain, 'example.com');
    assert.equal(record.destinationPort, 443, 'порт приходит строкой и должен привестись к числу');
    assert.equal(record.uploadBytes, 1200);
    assert.equal(record.downloadBytes, 340);
    assert.equal(record.network, 'tcp');
    assert.equal(record.inboundTag, 'tuic-in-own');
    assert.equal(record.startedAtUnixMs, Date.parse('2026-08-30T10:00:00.000Z'));
    assert.equal(record.closed, false);
  });

  it('пустой пользователь — это нормальная запись с фронта, а не брак', () => {
    // Xeon-фронт никого не аутентифицирует: он слепой SNI-релей. Зато источник у него настоящий —
    // тот самый, которого egress за кольцом уже не видит.
    const [record] = parseClashConnections(
      clashResponse([{ upload: 10, download: 20, metadata: { sourceIP: '203.0.113.77' } }]),
      false
    );

    assert.equal(record.user, '');
    assert.equal(record.sourceIp, '203.0.113.77');
  });

  it('помечает записи из буфера завершённых', () => {
    // Короткие потоки успевают открыться и закрыться между двумя опросами свипа — без них картина
    // смещалась бы к долгим соединениям, то есть мимо всплесков.
    const [record] = parseClashConnections(clashResponse([{ metadata: {} }]), true);

    assert.equal(record.closed, true);
  });

  it('кривое поле роняет в ноль одну запись, а не весь ответ', () => {
    const records = parseClashConnections(
      clashResponse([
        { upload: 'не число', download: null, metadata: { destinationPort: 'abc' } },
        { upload: 500, download: 100, metadata: { destinationPort: 80 } },
      ]),
      false
    );

    assert.equal(records.length, 2, 'вторая запись не должна пострадать из-за первой');
    assert.equal(records[0].uploadBytes, 0);
    assert.equal(records[0].destinationPort, 0);
    assert.equal(records[1].uploadBytes, 500);
    assert.equal(records[1].destinationPort, 80);
  });

  it('непарсящееся время становится нулём, а не NaN', () => {
    const [record] = parseClashConnections(clashResponse([{ start: 'вчера', metadata: {} }]), false);

    assert.equal(record.startedAtUnixMs, 0);
  });

  it('ответ без массива соединений даёт пустой список', () => {
    assert.deepEqual(parseClashConnections({}, false), []);
    assert.deepEqual(parseClashConnections({ connections: null }, false), []);
    assert.deepEqual(parseClashConnections(null, false), []);
  });
});

describe('чтение точки доступа из применённого конфига', () => {
  async function withConfig(content: string, run: (p: string) => Promise<void>): Promise<void> {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'clash-endpoint-'));
    const file = path.join(dir, 'config.json');
    await fs.writeFile(file, content, 'utf-8');
    try {
      await run(file);
    } finally {
      await fs.rm(dir, { recursive: true, force: true });
    }
  }

  it('достаёт адрес и секрет', async () => {
    await withConfig(
      JSON.stringify({ experimental: { clash_api: { external_controller: '127.0.0.1:28080', secret: 'abc' } } }),
      async (file) => {
        assert.deepEqual(await readClashApiEndpoint(file), { address: '127.0.0.1:28080', secret: 'abc' });
      }
    );
  });

  it('конфиг без блока — это «ещё не раскатано», а не ошибка', async () => {
    await withConfig(JSON.stringify({ inbounds: [] }), async (file) => {
      assert.equal(await readClashApiEndpoint(file), null);
    });
  });

  it('битый конфиг не роняет чтение', async () => {
    await withConfig('{ это не json', async (file) => {
      assert.equal(await readClashApiEndpoint(file), null);
    });
  });

  it('отсутствующий файл не роняет чтение', async () => {
    assert.equal(await readClashApiEndpoint('/nonexistent/config.json'), null);
  });
});
