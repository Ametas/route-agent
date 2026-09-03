import test from 'node:test';
import assert from 'node:assert';
import * as http from 'node:http';
import {
  currentSelection,
  decideWarpSelection,
  selectWarpMember,
  DIRECT_TAG,
  WARP_POOL_TAG,
  WARP_SELECTOR_TAG,
} from '../src/utils/rearWarpGuard.js';

/**
 * Сторож WARP-ветки тыла — замена автофолбека, который был у балансировщика xray (`fallbackTag`) и
 * которого у `urltest` в sing-box нет вовсе: при отсутствии истории замеров его `Select` возвращает
 * первого попавшегося участника, то есть трафик уходит в мёртвый туннель.
 *
 * Форма ответа `/proxies` и форма запроса на переключение взяты из исходников sing-box v1.14.0, а
 * не из головы: `history` — массив `{ time, delay }`, переключение — `PUT /proxies/{name}` с телом
 * `{"name": "<член>"}`, цель обязана быть селектором.
 */

const ALIVE = { history: [{ time: '2026-09-03T04:00:00.000Z', delay: 70 }] };
const DEAD = { history: [] };

function payload(over: Record<string, unknown> = {}): unknown {
  return {
    proxies: {
      direct: DEAD,
      [WARP_POOL_TAG]: ALIVE,
      [WARP_SELECTOR_TAG]: { now: WARP_POOL_TAG, all: [WARP_POOL_TAG, DIRECT_TAG], history: [] },
      'warp-key-1': ALIVE,
      ...over,
    },
  };
}

test('decideWarpSelection', async (t) => {
  await t.test('keeps the pool while at least one key answers', () => {
    const decision = decideWarpSelection(payload());

    assert.strictEqual(decision?.desired, WARP_POOL_TAG);
    assert.strictEqual(decision?.aliveKeys, 1);
  });

  await t.test('falls back to direct when every key is dead', () => {
    /**
     * Ровно тот случай, ради которого сторож существует. Пустая история — это «не ответил», а не
     * «нет данных»: sing-box УДАЛЯЕТ запись при неудачном замере и пишет при удачном.
     */
    const decision = decideWarpSelection(payload({ 'warp-key-1': DEAD, 'warp-key-2': DEAD }));

    assert.strictEqual(decision?.desired, DIRECT_TAG);
    assert.strictEqual(decision?.aliveKeys, 0);
    assert.strictEqual(decision?.totalKeys, 2);
  });

  await t.test('comes back to the pool as soon as one key revives', () => {
    // Возврат так же важен, как уход: иначе одна неудачная минута оставила бы ноду без WARP до
    // следующей перезагрузки конфига.
    const decision = decideWarpSelection(payload({ 'warp-key-1': DEAD, 'warp-key-2': ALIVE }));

    assert.strictEqual(decision?.desired, WARP_POOL_TAG);
    assert.strictEqual(decision?.aliveKeys, 1);
  });

  await t.test('does not judge the pool group itself as a key', () => {
    /**
     * Ловушка именования: разбор считает ключом всё, что начинается с `warp-` и имеет непустой
     * остаток. Поэтому группа автоподбора и названа `wg-pool`. Живая группа при мёртвых ключах не
     * должна выдавать себя за живой ключ — иначе фолбек не сработал бы никогда.
     */
    const decision = decideWarpSelection(payload({ 'warp-key-1': DEAD, [WARP_POOL_TAG]: ALIVE }));

    assert.strictEqual(decision?.desired, DIRECT_TAG, 'группа посчитана за живой ключ');
  });

  await t.test('stays out of it when there is no selector at all', () => {
    // Так выглядит нода без WARP или с пустым пулом: генератор тогда не создаёт ни группу, ни
    // селектор. Вмешиваться не во что, и «нет ключей → direct» здесь было бы неверно.
    const withoutSelector = { proxies: { direct: DEAD, 'warp-key-1': DEAD } };

    assert.strictEqual(decideWarpSelection(withoutSelector), null);
  });

  await t.test('survives a malformed payload instead of throwing', () => {
    for (const bad of [null, undefined, {}, { proxies: null }, 'нет', []]) {
      assert.strictEqual(decideWarpSelection(bad), null);
    }
  });
});

test('currentSelection', async (t) => {
  await t.test('reads what the selector points at right now', () => {
    assert.strictEqual(currentSelection(payload()), WARP_POOL_TAG);
    assert.strictEqual(
      currentSelection(payload({ [WARP_SELECTOR_TAG]: { now: DIRECT_TAG } })),
      DIRECT_TAG
    );
  });

  await t.test('returns null when the selector or the field is missing', () => {
    assert.strictEqual(currentSelection({ proxies: {} }), null);
    assert.strictEqual(currentSelection(payload({ [WARP_SELECTOR_TAG]: {} })), null);
    assert.strictEqual(currentSelection(null), null);
  });
});

test('selectWarpMember', async (t) => {
  await t.test('sends the exact request shape sing-box accepts', async () => {
    /**
     * Сверено по исходникам: `PUT /proxies/{name}`, тело `{"name": "<член>"}`, успех — 204 без
     * тела. Ошибись в методе или в имени поля — sing-box ответил бы 400, и фолбек молча не работал
     * бы: селектор остался бы на мёртвом пуле.
     */
    let seen: { method?: string; url?: string; body?: string; auth?: string } = {};

    const server = http.createServer((req, res) => {
      let body = '';
      req.on('data', (chunk) => (body += chunk));
      req.on('end', () => {
        seen = { method: req.method, url: req.url, body, auth: req.headers.authorization };
        res.statusCode = 204;
        res.end();
      });
    });
    await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
    const port = (server.address() as { port: number }).port;

    try {
      await selectWarpMember({ address: `127.0.0.1:${port}`, secret: 's3cret' }, DIRECT_TAG);

      assert.strictEqual(seen.method, 'PUT');
      assert.strictEqual(seen.url, `/proxies/${WARP_SELECTOR_TAG}`);
      assert.deepStrictEqual(JSON.parse(seen.body ?? '{}'), { name: DIRECT_TAG });
      assert.strictEqual(seen.auth, 'Bearer s3cret');
    } finally {
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  });

  await t.test('raises when the API refuses the switch', async () => {
    // 400 отвечает sing-box, когда цель не селектор или член не входит в группу. Проглотить это
    // значило бы считать фолбек выполненным, когда он не выполнен.
    const server = http.createServer((_req, res) => {
      res.statusCode = 400;
      res.end('{"message":"Must be a Selector"}');
    });
    await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
    const port = (server.address() as { port: number }).port;

    try {
      await assert.rejects(
        () => selectWarpMember({ address: `127.0.0.1:${port}`, secret: '' }, WARP_POOL_TAG),
        /400/
      );
    } finally {
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  });
});
