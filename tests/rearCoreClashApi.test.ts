import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { readActiveRearClashApi, MIHOMO_REAR_CORE, SINGBOX_REAR_CORE } from '../src/utils/rearCore.js';
import { decideWarpSelection } from '../src/utils/rearWarpGuard.js';
import { parseWarpKeyHealth } from '../src/utils/warpKeyHealth.js';

process.env.NODE_ENV = 'test';

/**
 * Локальные потребители Clash API тыла — сторож WARP и отчёт о здоровье ключей.
 *
 * ЖИВОЙ СЛУЧАЙ 2026-09-06. Оба читали конфиг тыла по пути sing-box и разбирали его форму
 * (`experimental.clash_api`). После перевода узла на mihomo этого файла на месте нет, а у нового
 * конфига поля лежат на верхнем уровне и называются иначе — оба потребителя перестали работать
 * МОЛЧА: `null` из читалки они трактуют как «тыл не настроен» и просто ничего не делают.
 */

async function withCore<T>(
  which: 'mihomo' | 'singbox' | 'none',
  body: () => Promise<T>
): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-api-'));
  const originals = { mc: config.REAR_MIHOMO_CONFIG_PATH, sc: config.REAR_SINGBOX_CONFIG_PATH };
  config.REAR_MIHOMO_CONFIG_PATH = path.join(dir, 'rear.yaml');
  config.REAR_SINGBOX_CONFIG_PATH = path.join(dir, 'rear.json');

  if (which === 'mihomo') {
    await fs.writeFile(
      config.REAR_MIHOMO_CONFIG_PATH,
      JSON.stringify({ 'external-controller': '127.0.0.1:28081', secret: 'mihomo-secret' }),
      'utf-8'
    );
  }
  if (which === 'singbox') {
    await fs.writeFile(
      config.REAR_SINGBOX_CONFIG_PATH,
      JSON.stringify({ experimental: { clash_api: { external_controller: '127.0.0.1:28081', secret: 'sb-secret' } } }),
      'utf-8'
    );
  }

  try {
    return await body();
  } finally {
    config.REAR_MIHOMO_CONFIG_PATH = originals.mc;
    config.REAR_SINGBOX_CONFIG_PATH = originals.sc;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('конфиг mihomo читается по своей форме, с полями верхнего уровня', async () => {
  await withCore('mihomo', async () => {
    const active = await readActiveRearClashApi();

    assert.ok(active, 'адрес Clash API не прочитался — сторож WARP останется без дела');
    assert.strictEqual(active!.core.id, 'mihomo');
    assert.strictEqual(active!.address, '127.0.0.1:28081');
    assert.strictEqual(active!.secret, 'mihomo-secret');
  });
});

test('конфиг sing-box читается по-прежнему, из experimental.clash_api', async () => {
  await withCore('singbox', async () => {
    const active = await readActiveRearClashApi();

    assert.ok(active);
    assert.strictEqual(active!.core.id, 'singbox');
    assert.strictEqual(active!.secret, 'sb-secret');
  });
});

test('без конфига тыла читалка честно отвечает «нечего читать»', async () => {
  await withCore('none', async () => {
    assert.strictEqual(await readActiveRearClashApi(), null);
  });
});

/**
 * Имена членов селектора у ядер РАЗНЫЕ. PUT с чужим именем mihomo отвергает — то есть сторож
 * перестал бы переключать вообще, и это не проявилось бы ничем, кроме отсутствия эффекта.
 */
test('сторож выбирает членов селектора по имени того ядра, что работает', () => {
  const alive = {
    proxies: {
      warp: { now: 'warp-auto' },
      'warp-0001': { history: [{ delay: 120, time: new Date().toISOString() }] },
    },
  };
  const dead = {
    proxies: {
      warp: { now: 'warp-auto' },
      'warp-0001': { history: [] },
    },
  };

  assert.strictEqual(decideWarpSelection(alive, MIHOMO_REAR_CORE.warpMembers)?.desired, 'warp-auto');
  assert.strictEqual(decideWarpSelection(dead, MIHOMO_REAR_CORE.warpMembers)?.desired, 'DIRECT');

  assert.strictEqual(decideWarpSelection(alive, SINGBOX_REAR_CORE.warpMembers)?.desired, 'wg-pool');
  assert.strictEqual(decideWarpSelection(dead, SINGBOX_REAR_CORE.warpMembers)?.desired, 'direct');
});

/**
 * `warp-auto` — группа автоматического отката, а не туннель. Под префикс `warp-` она попадает, и
 * без исключения уезжала бы в оркестратор как несуществующий ключ со своей задержкой.
 */
test('группа warp-auto не считается WARP-ключом', () => {
  const payload = {
    proxies: {
      'warp-auto': { history: [{ delay: 90, time: new Date().toISOString() }] },
      'warp-0001': { history: [{ delay: 120, time: new Date().toISOString() }] },
      'wg-pool': { history: [{ delay: 100, time: new Date().toISOString() }] },
    },
  };

  const tags = parseWarpKeyHealth(payload).map((record) => record.endpointTag);

  assert.deepStrictEqual(tags, ['warp-0001'], `в отчёт попали лишние записи: ${JSON.stringify(tags)}`);
});
