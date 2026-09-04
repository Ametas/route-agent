import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config, configSchema } from '../src/config.js';
import { configureRearSingboxHandler } from '../src/services/rearSingbox.service.js';

process.env.NODE_ENV = 'test';

/**
 * Переезд конфига тыла из общего каталога sing-box.
 *
 * ЗАЧЕМ ПЕРЕЕХАЛИ. Пакетный юнит sing-box запускается с флагом `-C /etc/sing-box`, а это КАТАЛОГ:
 * sing-box сливает в один конфиг все `*.json` оттуда. Пока `rear.json` лежал рядом с фронтовым
 * `config.json`, один запуск пакетным юнитом склеил бы их — WARP-эндпоинты и loopback-инбаунды тыла
 * оказались бы в одном процессе с публичными слушателями фронта. Найдено на ноде mo-nl-node
 * (2026-09-04), где пакетный sing-box и наш тыл сосуществовали три недели.
 */

const REAR_CONFIG = {
  log: { level: 'warn' },
  inbounds: [{ type: 'vless', tag: 'stars-in', listen: '127.0.0.1', listen_port: 29000 }],
  outbounds: [{ type: 'direct', tag: 'direct' }],
};

interface RearResponse {
  success: boolean;
  message: string;
  running: boolean;
}

async function callHandler(): Promise<RearResponse> {
  return new Promise((resolve, reject) => {
    const call = {
      metadata: { get: () => [config.EGRESS_CONTROL_SECRET] },
      request: { enabled: true, configJson: JSON.stringify(REAR_CONFIG) },
    };
    void configureRearSingboxHandler(call as never, ((err: unknown, res: RearResponse) => {
      if (err) reject(err);
      else resolve(res);
    }) as never);
  });
}

test('умолчание пути уводит конфиг тыла из каталога sing-box', () => {
  /**
   * Сторож на само значение, а не на поведение: вернуть путь обратно в /etc/sing-box — это вернуть
   * ловушку, которая три недели держала ноду без единого слушателя на UDP 443.
   */
  const parsed = configSchema.parse({ EGRESS_CONTROL_SECRET: 'x' });

  assert.ok(
    !parsed.REAR_SINGBOX_CONFIG_PATH.startsWith('/etc/sing-box/'),
    `конфиг тыла снова в общем каталоге: ${parsed.REAR_SINGBOX_CONFIG_PATH}`
  );
});

test('старый файл убирается после успешной записи нового', async () => {
  /**
   * Оставленный на прежнем месте, он продолжал бы подмешиваться в фронтовой конфиг — то есть
   * переезд без уборки не решал бы ровно ничего.
   *
   * Удаление идёт ПОСЛЕ записи нового: сорвись запись, нода осталась бы вовсе без конфига тыла.
   */
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-move-'));
  const originals = {
    configPath: config.REAR_SINGBOX_CONFIG_PATH,
    unitPath: config.REAR_SINGBOX_UNIT_FILE_PATH,
  };
  config.REAR_SINGBOX_CONFIG_PATH = path.join(dir, 'rear.json');
  config.REAR_SINGBOX_UNIT_FILE_PATH = path.join(dir, 'route-rear-singbox.service');

  // Старое место подделать нельзя — путь в коде зашит константой, — поэтому проверяем то, что
  // проверяемо: новый файл появился, а обработчик не упал на попытке убрать отсутствующий старый.
  try {
    const result = await callHandler();

    assert.strictEqual(result.success, true, result.message);
    assert.ok(await fs.readFile(config.REAR_SINGBOX_CONFIG_PATH, 'utf-8'));
  } finally {
    config.REAR_SINGBOX_CONFIG_PATH = originals.configPath;
    config.REAR_SINGBOX_UNIT_FILE_PATH = originals.unitPath;
    await fs.rm(dir, { recursive: true, force: true });
  }
});

test('сменившийся юнит заставляет писать конфиг заново, а не отвечать «уже актуально»', async () => {
  /**
   * САМОЕ ВАЖНОЕ В ПЕРЕЕЗДЕ. При смене пути меняется `ExecStart`, а перезагрузка тут бессильна: по
   * SIGHUP sing-box перечитывает ТОТ путь, с которым его запустили, — у работающего процесса он
   * прежний. Ответить «уже актуально» и уйти значило бы считать, что новый конфиг применён, когда
   * процесс его даже не видел.
   */
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rear-unit-'));
  const originals = {
    configPath: config.REAR_SINGBOX_CONFIG_PATH,
    unitPath: config.REAR_SINGBOX_UNIT_FILE_PATH,
  };
  config.REAR_SINGBOX_CONFIG_PATH = path.join(dir, 'rear.json');
  config.REAR_SINGBOX_UNIT_FILE_PATH = path.join(dir, 'route-rear-singbox.service');

  try {
    const first = await callHandler();
    assert.strictEqual(first.success, true, first.message);
    // Второй вызов с тем же конфигом И тем же юнитом — вот теперь «уже актуально» законно.
    const second = await callHandler();
    assert.match(second.message, /already current/);

    // Портим юнит, изображая смену ExecStart: следующий вызов обязан снова записать конфиг.
    await fs.writeFile(config.REAR_SINGBOX_UNIT_FILE_PATH, '[Unit]\nDescription=stale\n', 'utf-8');
    const third = await callHandler();

    assert.doesNotMatch(third.message, /already current/, 'смену юнита сочли за «ничего не изменилось»');
  } finally {
    config.REAR_SINGBOX_CONFIG_PATH = originals.configPath;
    config.REAR_SINGBOX_UNIT_FILE_PATH = originals.unitPath;
    await fs.rm(dir, { recursive: true, force: true });
  }
});
