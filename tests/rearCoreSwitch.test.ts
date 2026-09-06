import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { configureRearSingboxHandler } from '../src/services/rearSingbox.service.js';

// Без этого обработчик упирается в проверку наличия бинаря и отвечает `*_not_installed`, не дойдя
// до самой логики; заодно короткое замыкание проходит валидацию конфига настоящим ядром.
process.env.NODE_ENV = 'test';

/**
 * Переключение тылового ядра.
 *
 * ГЛАВНАЯ ОПАСНОСТЬ, ради которой всё это проверяется: оба ядра слушают одни и те же порты петли
 * (29000/29001). Оставленный работать предшественник не даёт новому подняться вовсе, а выглядит это
 * как «конфиг не применился» — без единого слова про настоящую причину.
 */

interface RearResponse {
  success: boolean;
  message: string;
  running: boolean;
  skippedReason?: string;
}

const REAR_CONFIG = { 'log-level': 'warning', mode: 'rule', rules: ['MATCH,DIRECT'] };

async function callHandler(core: string | undefined, enabled = true): Promise<RearResponse> {
  return new Promise((resolve, reject) => {
    const call = {
      metadata: { get: () => [config.EGRESS_CONTROL_SECRET] },
      request: { enabled, core, configJson: JSON.stringify(REAR_CONFIG) },
    };
    void configureRearSingboxHandler(call as never, ((err: unknown, res: RearResponse) => {
      if (err) reject(err);
      else resolve(res);
    }) as never);
  });
}

const exists = (p: string) => fs.stat(p).then(() => true).catch(() => false);

async function withPaths<T>(body: (paths: {
  singboxConfig: string; singboxUnit: string; mihomoConfig: string; mihomoUnit: string;
}) => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-core-'));
  const originals = {
    sc: config.REAR_SINGBOX_CONFIG_PATH,
    su: config.REAR_SINGBOX_UNIT_FILE_PATH,
    mc: config.REAR_MIHOMO_CONFIG_PATH,
    mu: config.REAR_MIHOMO_UNIT_FILE_PATH,
  };
  const paths = {
    singboxConfig: path.join(dir, 'rear.json'),
    singboxUnit: path.join(dir, 'route-rear-singbox.service'),
    mihomoConfig: path.join(dir, 'rear.yaml'),
    mihomoUnit: path.join(dir, 'route-rear-mihomo.service'),
  };
  config.REAR_SINGBOX_CONFIG_PATH = paths.singboxConfig;
  config.REAR_SINGBOX_UNIT_FILE_PATH = paths.singboxUnit;
  config.REAR_MIHOMO_CONFIG_PATH = paths.mihomoConfig;
  config.REAR_MIHOMO_UNIT_FILE_PATH = paths.mihomoUnit;
  try {
    return await body(paths);
  } finally {
    config.REAR_SINGBOX_CONFIG_PATH = originals.sc;
    config.REAR_SINGBOX_UNIT_FILE_PATH = originals.su;
    config.REAR_MIHOMO_CONFIG_PATH = originals.mc;
    config.REAR_MIHOMO_UNIT_FILE_PATH = originals.mu;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

/**
 * Пустое `core` — это умолчание proto3 у оркестратора, который поля ещё не присылает. Старое
 * поведение обязано остаться старым ПО УМОЛЧАНИЮ, а не по совпадению: иначе первый же деплой
 * агента увёл бы весь флот на ядро, которого на узлах ещё нет.
 */
test('без поля core тыл поднимается на sing-box, как раньше', async () => {
  await withPaths(async (paths) => {
    const response = await callHandler(undefined);

    assert.strictEqual(response.success, true);
    assert.strictEqual(await exists(paths.singboxConfig), true);
    assert.strictEqual(await exists(paths.mihomoConfig), false);
    assert.strictEqual(await exists(paths.mihomoUnit), false);
  });
});

test('core=mihomo пишет свой конфиг и свой юнит, не трогая пути sing-box', async () => {
  await withPaths(async (paths) => {
    const response = await callHandler('mihomo');

    assert.strictEqual(response.success, true);
    assert.match(response.message, /mihomo/);
    assert.strictEqual(await exists(paths.mihomoConfig), true);
    assert.strictEqual(await exists(paths.mihomoUnit), true);
    assert.strictEqual(await exists(paths.singboxConfig), false);
  });
});

/** Юнит должен запускать mihomo своим синтаксисом и перечитывать конфиг по SIGHUP, а не рестартом. */
test('юнит mihomo запускает его правильно и умеет перезагрузку без обрыва', async () => {
  await withPaths(async (paths) => {
    await callHandler('mihomo');
    const unit = await fs.readFile(paths.mihomoUnit, 'utf-8');

    assert.match(unit, /ExecStart=.*mihomo -d .* -f /);
    assert.match(unit, /ExecReload=.*-t -d .* -f .* && \/bin\/kill -HUP/);
  });
});

/**
 * Сердцевина: после переключения от предшественника не должно остаться НИ ЮНИТА, ни конфига.
 * Оставленный юнит — это работающий процесс на тех же портах.
 */
test('переключение на mihomo снимает тыл на sing-box целиком', async () => {
  await withPaths(async (paths) => {
    await callHandler(undefined);
    assert.strictEqual(await exists(paths.singboxUnit), true, 'подготовка: тыл на sing-box не поднялся');

    await callHandler('mihomo');

    assert.strictEqual(await exists(paths.singboxUnit), false, 'юнит sing-box остался');
    assert.strictEqual(await exists(paths.singboxConfig), false, 'конфиг sing-box остался');
    assert.strictEqual(await exists(paths.mihomoUnit), true);
    assert.strictEqual(await exists(paths.mihomoConfig), true);
  });
});

test('обратное переключение на sing-box снимает тыл на mihomo целиком', async () => {
  await withPaths(async (paths) => {
    await callHandler('mihomo');
    assert.strictEqual(await exists(paths.mihomoUnit), true, 'подготовка: тыл на mihomo не поднялся');

    await callHandler('singbox');

    assert.strictEqual(await exists(paths.mihomoUnit), false, 'юнит mihomo остался');
    assert.strictEqual(await exists(paths.mihomoConfig), false, 'конфиг mihomo остался');
    assert.strictEqual(await exists(paths.singboxUnit), true);
  });
});

/**
 * «Тыл выключен» — состояние УЗЛА, а не одного ядра. Снимать только запрошенное значило бы, что
 * выключение после переключения оставляет прежний инстанс работать: порты заняты, трафик по-прежнему
 * идёт в WARP, а флот считает тыл снятым.
 */
test('выключение снимает оба ядра, каким бы ни было запрошенное', async () => {
  await withPaths(async (paths) => {
    await callHandler('mihomo');
    // Юнит sing-box подкладываем руками: так выглядит узел, переключённый на mihomo, но с
    // недоубранным предшественником — например, если переключение оборвалось на полпути.
    await fs.writeFile(paths.singboxUnit, '[Unit]\n', 'utf-8');
    await fs.writeFile(paths.singboxConfig, '{}', 'utf-8');

    const response = await callHandler('mihomo', false);

    assert.strictEqual(response.success, true);
    assert.strictEqual(response.running, false);
    for (const p of [paths.mihomoUnit, paths.mihomoConfig, paths.singboxUnit, paths.singboxConfig]) {
      assert.strictEqual(await exists(p), false, `осталось: ${path.basename(p)}`);
    }
  });
});

/**
 * Конфиг тыла несёт приватные ключи WARP. Повторный пуш того же конфига не должен переписывать файл
 * и дёргать перезагрузку — цена перезагрузки здесь оборванные сессии всей звёздной ветки узла.
 */
test('повторный пуш того же конфига на mihomo ничего не переписывает', async () => {
  await withPaths(async (paths) => {
    await callHandler('mihomo');
    const firstWrite = (await fs.stat(paths.mihomoConfig)).mtimeMs;

    await new Promise((resolve) => setTimeout(resolve, 12));
    const response = await callHandler('mihomo');

    assert.strictEqual(response.success, true);
    assert.match(response.message, /already current/);
    assert.strictEqual((await fs.stat(paths.mihomoConfig)).mtimeMs, firstWrite);
  });
});
