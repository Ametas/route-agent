import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { applyConfigHandler } from '../src/services/config.service.js';

// Как и в остальных файлах тестов: без этого обработчик уходит в проверки, требующие живой ноды.
process.env.NODE_ENV = 'test';

/**
 * `atomicApplyAndReload` выполняет команду перезагрузки, если переменная задана ДАЖЕ в тестах
 * (`NODE_ENV !== 'test' || RELOAD_COMMAND`) — шов для тех тестов, которым нужно увидеть саму
 * команду. Здесь смотрят на mtime файла, а не на команду, поэтому шов снимаем: иначе прогон зовёт
 * настоящий `systemctl`, которого нет ни в CI, ни на машине разработчика.
 */
delete process.env.RELOAD_COMMAND;

/**
 * Пуш фронтового конфига, который ничего не меняет, не должен ничего делать.
 *
 * ЖИВОЙ СЛУЧАЙ 2026-09-17, ради которого это появилось. Удаление одного абонента в 07:39 оставило
 * на ноде 49 соединений из 144, и самое старое из выживших началось в 07:39:38 — то есть reload не
 * пережило НИ ОДНО. Причин было две, и обе здесь закрываются: оркестратор пушит конфиг веером на
 * весь флот (ноды, которых правка не касалась, платили обрывом ни за что), а удаление давало два
 * пуша подряд — собственный и от вебхука панели, — причём второй вёз байт в байт тот же конфиг.
 *
 * Reload у sing-box — это `instance.Close()` и сборка нового инстанса, частичной перезагрузки в
 * дереве нет (SagerNet/sing-box#3731). Значит единственный способ не рвать соединения — не звать
 * reload вовсе.
 */

interface ConfigResponse {
  success: boolean;
  message: string;
}

const EGRESS_CONFIG = {
  log: { level: 'warn' },
  inbounds: [{ type: 'vless', tag: 'stars-in', listen: '::', listen_port: 443 }],
  outbounds: [{ type: 'direct', tag: 'direct' }],
};

async function callHandler(configObj: object): Promise<ConfigResponse> {
  return new Promise((resolve, reject) => {
    const call = {
      metadata: { get: () => [config.EGRESS_CONTROL_SECRET] },
      request: { configJson: JSON.stringify(configObj) },
    };
    void applyConfigHandler(call as never, ((err: unknown, res: ConfigResponse) => {
      if (err) reject(err);
      else resolve(res);
    }) as never);
  });
}

async function withTempConfig(t: { after: (fn: () => Promise<void>) => void }): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'singbox-unchanged-'));
  const configPath = path.join(dir, 'config.json');
  const originalPath = config.SINGBOX_CONFIG_PATH;
  config.SINGBOX_CONFIG_PATH = configPath;

  t.after(async () => {
    config.SINGBOX_CONFIG_PATH = originalPath;
    delete process.env.SINGBOX_TEST_INACTIVE;
    await fs.rm(dir, { recursive: true, force: true });
  });

  return configPath;
}

test('байт в байт тот же конфиг не переписывается и не перезагружается', async (t) => {
  const configPath = await withTempConfig(t);

  const first = await callHandler(EGRESS_CONFIG);
  assert.strictEqual(first.success, true, first.message);
  const writtenAt = (await fs.stat(configPath)).mtimeMs;

  /**
   * Судим по времени изменения файла, а не по тексту ответа: текст — это то, что агент про себя
   * рассказывает, а mtime — то, что он на самом деле сделал.
   */
  await new Promise((r) => setTimeout(r, 20));
  const second = await callHandler(EGRESS_CONFIG);

  assert.strictEqual(second.success, true, second.message);
  assert.strictEqual((await fs.stat(configPath)).mtimeMs, writtenAt, 'конфиг переписан без изменений');
  assert.match(second.message, /already current/);
});

test('успех, а не пропуск: иначе PurgeWorker подвиснет навсегда', async (t) => {
  /**
   * `PurgeWorker` дочищает абонента из базы только после того, как КАЖДАЯ нода подтвердила пуш
   * (`allPushResults.every(isPushSuccessOrBenignSkip)`). Ответь агент на пропуск неуспехом — и на
   * флоте, где хоть одна нода уже имеет актуальный конфиг, очередь не разберётся никогда. Ровно
   * такая блокировка уже случалась в 2026-09-02 с нодами без sing-box.
   */
  await withTempConfig(t);

  await callHandler(EGRESS_CONFIG);
  const second = await callHandler(EGRESS_CONFIG);

  assert.strictEqual(second.success, true, 'пропуск отвечен неуспехом — очередь чистки встанет');
});

test('изменившийся конфиг записывается', async (t) => {
  const configPath = await withTempConfig(t);

  await callHandler(EGRESS_CONFIG);

  const withExtraInbound = {
    ...EGRESS_CONFIG,
    inbounds: [
      ...EGRESS_CONFIG.inbounds,
      { type: 'tuic', tag: 'lightning-in', listen: '::', listen_port: 8443 },
    ],
  };
  const result = await callHandler(withExtraInbound);

  assert.strictEqual(result.success, true, result.message);
  const onDisk = JSON.parse(await fs.readFile(configPath, 'utf-8'));
  assert.strictEqual(onDisk.inbounds.length, 2, 'изменившийся конфиг не записан');
  assert.doesNotMatch(result.message, /already current/);
});

test('тот же конфиг всё равно поднимает лежащее ядро', async (t) => {
  /**
   * Пропуск завязан на ДВА условия, и второе легко счесть лишним: конфиг не изменился И ядро живо.
   * Без второго нода, у которой sing-box лёг, осталась бы лежать — оркестратор возит ей тот же
   * самый конфиг, агент каждый раз отвечает «уже актуально», и поднять инстанс некому.
   *
   * Проверяется через `SINGBOX_TEST_INACTIVE`, потому что `systemctl` в тестах закорочен — тот же
   * приём, что у `REAR_TEST_INACTIVE` в тылу.
   */
  const configPath = await withTempConfig(t);

  await callHandler(EGRESS_CONFIG);
  const writtenAt = (await fs.stat(configPath)).mtimeMs;

  process.env.SINGBOX_TEST_INACTIVE = '1';
  await new Promise((r) => setTimeout(r, 20));
  const result = await callHandler(EGRESS_CONFIG);

  assert.doesNotMatch(result.message, /already current/, 'лежащее ядро сочли за «уже актуально»');
  assert.notStrictEqual(
    (await fs.stat(configPath)).mtimeMs,
    writtenAt,
    'конфиг не переписан, значит и запуска не было'
  );
});

test('первый пуш на чистую ноду не считается совпадением', async (t) => {
  /**
   * Файла на диске нет вовсе. Сравнение обязано ответить «не совпало», а не упасть и не счесть
   * отсутствие файла за совпадение: иначе нода, которой конфиг ещё ни разу не привозили, так его и
   * не получит.
   */
  const configPath = await withTempConfig(t);

  const result = await callHandler(EGRESS_CONFIG);

  assert.strictEqual(result.success, true, result.message);
  assert.doesNotMatch(result.message, /already current/);
  assert.ok(await fs.stat(configPath), 'конфиг не записан на чистую ноду');
});
