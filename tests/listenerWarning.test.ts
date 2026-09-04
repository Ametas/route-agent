import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import {
  CORE_NOT_LISTENING_KIND,
  detectCoreNotListening,
  parseListeningPorts,
  publicListenPorts,
} from '../src/utils/listenerWarning.js';

process.env.NODE_ENV = 'test';

/**
 * «Ядро живо, но никого не обслуживает».
 *
 * Нода mo-nl-node простояла так три недели: версия отдавалась, служба числилась активной, карточка
 * выглядела здоровой — а на публичном порту не слушал никто. Ни одна проверка этого не видела:
 * версия про файл, живость про службу, журнал про то, что процесс СКАЗАЛ. А он ничего не говорил.
 */

const SS_UDP = `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port
UNCONN 0      0            0.0.0.0:443        0.0.0.0:*
UNCONN 0      0          127.0.0.1:20000      0.0.0.0:*
`;

const SS_EMPTY = 'State  Recv-Q Send-Q Local Address:Port  Peer Address:Port\n';

async function withConfig<T>(configObj: unknown, body: () => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'listener-'));
  const configPath = path.join(dir, 'config.json');
  await fs.writeFile(configPath, JSON.stringify(configObj), 'utf-8');

  const original = config.SINGBOX_CONFIG_PATH;
  config.SINGBOX_CONFIG_PATH = configPath;
  try {
    return await body();
  } finally {
    config.SINGBOX_CONFIG_PATH = original;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('публичными считаются только инбаунды, доступные снаружи', () => {
  /**
   * Инбаунд на loopback — это диспетчер или тыл, и слушают они законно. Требовать их наличия
   * снаружи значило бы поднимать ложную тревогу на каждой здоровой ноде.
   */
  const ports = publicListenPorts({
    inbounds: [
      { type: 'hysteria2', listen: '::', listen_port: 443 },
      { type: 'vless', listen: '127.0.0.1', listen_port: 20000 },
      { type: 'tuic', listen_port: 8443 },
    ],
  });

  assert.deepStrictEqual(ports.sort((a, b) => a - b), [443, 8443]);
});

test('разбор ss берёт локальный порт, а не порт пира', () => {
  assert.deepStrictEqual(parseListeningPorts(SS_UDP).sort((a, b) => a - b), [443, 20000]);
});

test('молчит, когда всё в порядке', async () => {
  await withConfig({ inbounds: [{ listen: '::', listen_port: 443 }] }, async () => {
    const warning = await detectCoreNotListening(async () => ({ stdout: SS_UDP, stderr: '' }));
    assert.strictEqual(warning, null);
  });
});

test('докладывает, когда конфиг объявляет порт, а слушателя нет', async () => {
  /** Ровно тот случай: конфиг на месте, служба активна, на 443 пусто. */
  await withConfig({ inbounds: [{ listen: '::', listen_port: 443 }] }, async () => {
    const warning = await detectCoreNotListening(async () => ({ stdout: SS_EMPTY, stderr: '' }));

    assert.ok(warning);
    assert.strictEqual(warning!.kind, CORE_NOT_LISTENING_KIND);
    assert.match(warning!.sample, /443/);
  });
});

test('молчит на конфиге без публичных инбаундов и даже не зовёт ss', async () => {
  /**
   * Конфиг тыла состоит из loopback-инбаундов целиком — тревожиться там не о чем.
   *
   * Отдельно проверяется, что `ss` при этом НЕ вызывается. Без раннего выхода результат тот же
   * (сравнивать было бы не с чем), но мы дважды порождали бы процесс на каждом опросе каждой такой
   * ноды — мутационный прогон показал, что по одному лишь результату эта разница неразличима.
   */
  await withConfig({ inbounds: [{ listen: '127.0.0.1', listen_port: 29000 }] }, async () => {
    const calls: string[] = [];
    const warning = await detectCoreNotListening(async (command) => {
      calls.push(command);
      return { stdout: SS_EMPTY, stderr: '' };
    });

    assert.strictEqual(warning, null);
    assert.deepStrictEqual(calls, [], 'ss вызывался там, где сравнивать нечего');
  });
});

test('молчит, когда конфига нет вовсе', async () => {
  /** Ядро не установлено — это отдельный, уже видимый случай, и дублировать его тревогой незачем. */
  const original = config.SINGBOX_CONFIG_PATH;
  config.SINGBOX_CONFIG_PATH = path.join(os.tmpdir(), 'no-such-config-file.json');
  try {
    assert.strictEqual(await detectCoreNotListening(async () => ({ stdout: SS_EMPTY, stderr: '' })), null);
  } finally {
    config.SINGBOX_CONFIG_PATH = original;
  }
});

test('молчит, когда ss недоступен', async () => {
  /**
   * Отсутствие инструмента — это незнание, а не поломка. Доложить «не слушает» здесь значило бы
   * соврать, а ложная тревога приучает не читать доклады.
   */
  await withConfig({ inbounds: [{ listen: '::', listen_port: 443 }] }, async () => {
    const warning = await detectCoreNotListening(async () => {
      throw new Error('ss: command not found');
    });
    assert.strictEqual(warning, null);
  });
});
