import test from 'node:test';
import assert from 'node:assert';
import { EventEmitter } from 'node:events';
import * as fs from 'fs/promises';
import path from 'path';
import os from 'os';
import { config } from '../src/config.js';
import { receiveStreamedBinary, type ReceivedBinary } from '../src/services/binaryReceiver.js';
import { verifyMihomoAcceptsLiveConfig, uploadRearRuleSetHandler } from '../src/services/mihomo.service.js';

process.env.NODE_ENV = 'test';

/**
 * Общий приёмник стримов с бинарями. Раньше эта механика была скопирована в каждый обработчик
 * загрузки; теперь она одна, и потому её отказы стоит проверять отдельно — ошибка здесь касается
 * сразу всех будущих загрузок, а не одной.
 */

/** Поддельный клиентский стрим: ровно те события, на которые подписан приёмник. */
class FakeCall extends EventEmitter {
  public destroyed: Error | null = null;
  constructor(public metadata?: { get(key: string): unknown[] }) {
    super();
  }
  destroy(err: Error) {
    this.destroyed = err;
  }
}

const metadataWith = (secret: string) => ({ get: (key: string) => (key === 'x-orchestrator-secret' ? [secret] : []) });

/** Прогоняет стрим до конца и отдаёт ответ, который приёмник вернул в callback. */
async function drive(
  call: FakeCall,
  chunks: Array<Record<string, unknown>>,
  apply: (r: ReceivedBinary) => Promise<{ success: boolean; message: string }>
): Promise<{ success: boolean; message: string }> {
  return new Promise((resolve) => {
    void receiveStreamedBinary(
      call as never,
      ((_err: unknown, response: { success: boolean; message: string }) => resolve(response)) as never,
      { rpcName: 'TestUpload', tempPrefix: 'receiver-test' },
      apply
    );
    setImmediate(() => {
      for (const chunk of chunks) call.emit('data', chunk);
      call.emit('end');
    });
  });
}

test('принимает чанки, собирает файл целиком и отдаёт версию', async () => {
  const call = new FakeCall(metadataWith(config.EGRESS_CONTROL_SECRET));
  let seen: ReceivedBinary | null = null;
  let content = '';

  const response = await drive(
    call,
    [
      { version: '1.19.30', chunk: Buffer.from('первая') },
      { chunk: Buffer.from('-вторая') },
    ],
    async (received) => {
      seen = received;
      content = await fs.readFile(received.tempPath, 'utf8');
      return { success: true, message: 'ok' };
    }
  );

  assert.strictEqual(response.success, true);
  assert.strictEqual(content, 'первая-вторая');
  assert.strictEqual(seen!.version, '1.19.30');
  assert.strictEqual(seen!.bytes, Buffer.byteLength('первая-вторая'));
});

/**
 * Временный файл не должен пережить вызов: иначе каждая загрузка оставляла бы в `/tmp` копию
 * бинаря, а это десятки мегабайт на узел и утечка содержимого туда, где его никто не ждёт.
 */
test('временный файл удаляется после применения', async () => {
  const call = new FakeCall(metadataWith(config.EGRESS_CONTROL_SECRET));
  let tempPath = '';

  await drive(call, [{ chunk: Buffer.from('данные') }], async (received) => {
    tempPath = received.tempPath;
    return { success: true, message: 'ok' };
  });

  assert.strictEqual(await fs.stat(tempPath).then(() => true).catch(() => false), false);
});

/**
 * Секрет может прийти не в метаданных, а в самом чанке — так шлют часть клиентов. Отказ при этом
 * должен быть одинаковым, иначе один и тот же вызов принимался бы или отвергался в зависимости от
 * того, как именно его отправили.
 */
test('секрет из полезной нагрузки принимается наравне с метаданными', async () => {
  const call = new FakeCall(undefined);
  let applied = false;

  const response = await drive(
    call,
    [{ orchestratorSecret: config.EGRESS_CONTROL_SECRET, chunk: Buffer.from('x') }],
    async () => {
      applied = true;
      return { success: true, message: 'ok' };
    }
  );

  assert.strictEqual(response.success, true);
  assert.strictEqual(applied, true);
});

/**
 * При неверном секрете стрим не просто отвечает отказом, но и РВЁТСЯ: иначе отправитель продолжал
 * бы лить байты в никуда, а узел — принимать их на диск.
 */
test('неверный секрет отвергается и обрывает стрим, ничего не применяя', async () => {
  const call = new FakeCall(metadataWith('совершенно-не-тот-секрет'));
  let applied = false;

  const response = await drive(call, [{ chunk: Buffer.from('x') }], async () => {
    applied = true;
    return { success: true, message: 'ok' };
  });

  assert.strictEqual(response.success, false);
  assert.match(response.message, /Invalid orchestrator secret/);
  assert.strictEqual(applied, false);
  assert.ok(call.destroyed instanceof Error);
});

/**
 * Пустая загрузка — не «успешно применили ноль байт». Прежде такой файл дошёл бы до подмены бинаря
 * и оставил узел с нулевым исполняемым файлом.
 */
test('пустой стрим отвергается, а не применяется', async () => {
  const call = new FakeCall(metadataWith(config.EGRESS_CONTROL_SECRET));
  let applied = false;

  const response = await drive(call, [{ version: '1.0.0' }], async () => {
    applied = true;
    return { success: true, message: 'ok' };
  });

  assert.strictEqual(response.success, false);
  assert.match(response.message, /No binary data/);
  assert.strictEqual(applied, false);
});

test('отказ внутри apply превращается в ответ, а не в падение обработчика', async () => {
  const call = new FakeCall(metadataWith(config.EGRESS_CONTROL_SECRET));

  const response = await drive(call, [{ chunk: Buffer.from('x') }], async () => {
    throw new Error('диск переполнен');
  });

  assert.strictEqual(response.success, false);
  assert.match(response.message, /диск переполнен/);
});

/**
 * Бинарь mihomo приезжает РАНЬШЕ первой настройки тыла — это штатный порядок, сначала ядро, потом
 * конфигурация. Отсутствие конфига не должно читаться как «бинарь не годится».
 */
test('проверка конфига mihomo не блокирует установку, когда конфига ещё нет', async () => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'mihomo-cfg-'));
  const original = config.REAR_MIHOMO_CONFIG_PATH;
  config.REAR_MIHOMO_CONFIG_PATH = path.join(dir, 'rear.yaml');

  try {
    assert.deepStrictEqual(await verifyMihomoAcceptsLiveConfig('/bin/true'), { ok: true });
  } finally {
    config.REAR_MIHOMO_CONFIG_PATH = original;
    await fs.rm(dir, { recursive: true, force: true });
  }
});

// --- приём наборов правил ------------------------------------------------------------------

/** Прогоняет UploadRearRuleSet и возвращает ответ. */
async function driveRuleSet(name: string, body: string): Promise<{ success: boolean; message: string }> {
  const call = new FakeCall(metadataWith(config.EGRESS_CONTROL_SECRET));
  return new Promise((resolve) => {
    void uploadRearRuleSetHandler(
      call as never,
      ((_err: unknown, response: { success: boolean; message: string }) => resolve(response)) as never
    );
    setImmediate(() => {
      call.emit('data', { targetBinary: name, chunk: Buffer.from(body) });
      call.emit('end');
    });
  });
}

async function withRuleDir<T>(body: (dir: string) => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'rules-'));
  const original = config.REAR_RULE_SET_DIR;
  config.REAR_RULE_SET_DIR = path.join(dir, 'rules');
  try {
    return await body(config.REAR_RULE_SET_DIR);
  } finally {
    config.REAR_RULE_SET_DIR = original;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('набор записывается под своим именем и без временных остатков', async () => {
  await withRuleDir(async (dir) => {
    const response = await driveRuleSet('category-media', 'полезная-нагрузка');

    assert.strictEqual(response.success, true);
    assert.strictEqual(await fs.readFile(path.join(dir, 'category-media.mrs'), 'utf8'), 'полезная-нагрузка');
    // Промежуточный файл переименован, а не оставлен рядом.
    assert.deepStrictEqual(await fs.readdir(dir), ['category-media.mrs']);
  });
});

/** Восклицательный знак у MetaCubeX помечает наборы «без китайского сегмента» — их большинство. */
test('имя с восклицательным знаком принимается', async () => {
  await withRuleDir(async (dir) => {
    const response = await driveRuleSet('category-ai-!cn', 'x');

    assert.strictEqual(response.success, true);
    assert.ok((await fs.readdir(dir)).includes('category-ai-!cn.mrs'));
  });
});

/**
 * Имя приезжает ПО СЕТИ и становится частью пути. Здесь проверяется не «отвергли ли строку», а то,
 * что за пределами каталога наборов не появилось НИЧЕГО.
 */
test('обход каталога отвергается, и наружу ничего не пишется', async () => {
  for (const evil of ['../../etc/passwd', '..', 'a/b', '/etc/passwd', '', '.hidden']) {
    await withRuleDir(async (dir) => {
      const parent = path.dirname(dir);
      const before = await fs.readdir(parent);

      const response = await driveRuleSet(evil, 'вредоносное');

      assert.strictEqual(response.success, false, `имя ${JSON.stringify(evil)} приняли`);
      assert.match(response.message, /Недопустимое имя/);
      assert.deepStrictEqual(await fs.readdir(parent), before, `имя ${JSON.stringify(evil)} что-то создало`);
      assert.strictEqual(await fs.stat(dir).then(() => true).catch(() => false), false);
    });
  }
});

test('слишком длинное имя отвергается', async () => {
  await withRuleDir(async () => {
    const response = await driveRuleSet('a'.repeat(200), 'x');
    assert.strictEqual(response.success, false);
  });
});
