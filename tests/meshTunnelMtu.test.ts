import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import { configureMeshTunnelHandler } from '../src/services/meshTunnel.service.js';

process.env.NODE_ENV = 'test';

/**
 * MTU меш-туннеля.
 *
 * ЗАЧЕМ ОН ВООБЩЕ ПОЯВИЛСЯ. `awg-quick` без указаний ставит 1420, исходя из честных 1500 под
 * туннелем. У наших провайдеров их нет: замер на боевом узле 2026-09-06 дал путевой MTU 1452 до
 * соседей, а внешние пакеты выходили 1492 — ядро резало каждый надвое, и фрагментами шло 27%
 * трафика к соседу.
 *
 * ⚠️ ЧЕГО ЗДЕСЬ НЕТ. Ветка «сменился MTU — нужен перезапуск, а не reload» живёт под
 * `NODE_ENV !== 'test'` и отсюда недостижима. Проверяется только то, что попадает в файл. Разница
 * важная: `awg-quick strip` выбрасывает MTU перед `awg syncconf`, поэтому через reload новое
 * значение не применится, и на диске оно будет новым, а на интерфейсе старым.
 */

interface MeshResponse {
  success: boolean;
  message: string;
}

async function callHandler(extra: Record<string, unknown>): Promise<{ response: MeshResponse; config: string }> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'mesh-mtu-'));
  const original = config.MESH_AWG_CONFIG_PATH;
  config.MESH_AWG_CONFIG_PATH = path.join(dir, 'awgmesh0.conf');

  try {
    const response = await new Promise<MeshResponse>((resolve, reject) => {
      const call = {
        metadata: { get: () => [config.EGRESS_CONTROL_SECRET] },
        request: {
          privateKey: 'cHJpdmF0ZS1rZXktYmFzZTY0LXBhZGRpbmc9',
          addressV4: '100.100.0.4/16',
          addressV6: 'fd00:a002::4/64',
          listenPort: 51821,
          peers: [],
          ...extra,
        },
      };
      void configureMeshTunnelHandler(call as never, ((err: unknown, res: MeshResponse) => {
        if (err) reject(err);
        else resolve(res);
      }) as never);
    });

    const written = await fs.readFile(config.MESH_AWG_CONFIG_PATH, 'utf-8').catch(() => '');
    return { response, config: written };
  } finally {
    config.MESH_AWG_CONFIG_PATH = original;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('присланный MTU попадает в конфиг интерфейса', async () => {
  const { response, config: written } = await callHandler({ mtu: 1380 });

  assert.strictEqual(response.success, true, response.message);
  assert.match(written, /^MTU = 1380$/m, `MTU не записан:\n${written}`);
});

/**
 * Ноль и отсутствие поля означают «оставить умолчание awg-quick». Записать `MTU = 0` значило бы
 * поднять интерфейс с нулевым MTU — то есть не поднять его вовсе.
 */
test('без MTU и при нуле строка не пишется вовсе', async () => {
  for (const extra of [{}, { mtu: 0 }, { mtu: -1 }]) {
    const { config: written } = await callHandler(extra);
    assert.doesNotMatch(written, /^MTU\s*=/m, `для ${JSON.stringify(extra)} записалось:\n${written}`);
  }
});

/**
 * Значение приходит по сети и попадает в файл конфигурации, который исполняет `awg-quick`. Дробное
 * или строковое значение там означало бы либо отказ поднять интерфейс, либо — хуже — подстановку
 * чужого текста в конфиг.
 */
test('нецелые и нечисловые значения не доезжают до конфига в сыром виде', async () => {
  const { config: fractional } = await callHandler({ mtu: 1380.7 });
  assert.match(fractional, /^MTU = 1380$/m, 'дробное значение не усечено');

  const { config: garbage } = await callHandler({ mtu: '1380\nPostUp = curl evil' });
  assert.doesNotMatch(garbage, /PostUp/, `в конфиг попал посторонний текст:\n${garbage}`);
});
