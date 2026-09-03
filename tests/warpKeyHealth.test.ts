import test from 'node:test';
import assert from 'node:assert/strict';
import { parseWarpKeyHealth } from '../src/utils/warpKeyHealth.js';

/**
 * Разбор ответа `/proxies` тылового sing-box.
 *
 * Форма взята из исходников sing-box v1.14.0, а не придумана: `getProxies` кладёт всё под ключ
 * `proxies`, каждому прокси даёт `history` в виде массива `{ time, delay }` (типы `time.Time` и
 * `uint16` в `adapter.URLTestHistory`), и в этот же список ДОПИСЫВАЕТ endpoint-ы — иначе наших
 * WARP-туннелей там не было бы вовсе.
 */

function proxies(entries: Record<string, unknown>): unknown {
  return { proxies: entries };
}

function history(delay: number, time = '2026-09-03T04:00:00.000Z'): unknown {
  return { history: [{ time, delay }] };
}

test('parseWarpKeyHealth', async (t) => {
  await t.test('reads the stored delay of a WARP endpoint', () => {
    const records = parseWarpKeyHealth(
      proxies({ 'warp-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee': history(87) })
    );

    assert.strictEqual(records.length, 1);
    assert.strictEqual(records[0].endpointTag, 'warp-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee');
    assert.strictEqual(records[0].alive, true);
    assert.strictEqual(records[0].rttMs, 87);
    assert.strictEqual(records[0].measuredAtUnixMs, Date.parse('2026-09-03T04:00:00.000Z'));
  });

  await t.test('treats an EMPTY history as "did not answer", not as missing data', () => {
    /**
     * Не наше соглашение, а поведение sing-box: при неудачном замере он УДАЛЯЕТ запись истории, а
     * при удачном записывает. Прочитай мы пустую историю как «нет данных» — задушенный ключ
     * молча остался бы в обороте.
     */
    const records = parseWarpKeyHealth(proxies({ 'warp-key-1': { history: [] } }));

    assert.strictEqual(records.length, 1, 'ключ с пустой историей выпал из отчёта');
    assert.strictEqual(records[0].alive, false);
    assert.strictEqual(records[0].rttMs, 0);
  });

  await t.test('reports a key whose history field is absent entirely', () => {
    const records = parseWarpKeyHealth(proxies({ 'warp-key-1': {} }));

    assert.strictEqual(records.length, 1);
    assert.strictEqual(records[0].alive, false);
  });

  await t.test('reports only WARP endpoints, not the group or the other outbounds', () => {
    /**
     * Группа называется ровно `warp` и отсекается уже проверкой ПРЕФИКСА (`warp-`), а не длины —
     * я сначала приписал эту заслугу проверке длины, и мутационный прогон это опроверг.
     */
    const records = parseWarpKeyHealth(
      proxies({ warp: history(40), direct: history(10), GLOBAL: history(5), 'warp-key-1': history(50) })
    );

    assert.deepStrictEqual(records.map((r) => r.endpointTag), ['warp-key-1']);
  });

  await t.test('skips a tag that is the prefix with an EMPTY id after it', () => {
    /**
     * Вот что на самом деле стережёт проверка длины. Тег `warp-` дал бы пустой идентификатор
     * ключа, а оркестратор ищет по нему строку в базе: пустая строка не совпадёт ни с чем, и в
     * лучшем случае отчёт потеряется молча. Наш генератор такого тега не производит — но он
     * приходит из чужого процесса, и полагаться на это нельзя.
     */
    const records = parseWarpKeyHealth(proxies({ 'warp-': history(40), 'warp-key-1': history(50) }));

    assert.deepStrictEqual(records.map((r) => r.endpointTag), ['warp-key-1']);
  });

  await t.test('ignores a zero or negative delay as a non-measurement', () => {
    // Clash отдаёт delay нулём, когда замера фактически не было; принять его за 0 мс означало бы
    // объявить мёртвый ключ самым быстрым на ноде — и отдать ему весь трафик.
    const records = parseWarpKeyHealth(proxies({ 'warp-key-1': history(0) }));

    assert.strictEqual(records[0].alive, false);
  });

  await t.test('keeps the delay when the timestamp is unparseable', () => {
    // Задержка здесь важнее времени: без времени замер всё ещё говорит, что туннель отвечает.
    const records = parseWarpKeyHealth(proxies({ 'warp-key-1': history(60, 'not-a-date') }));

    assert.strictEqual(records[0].alive, true);
    assert.strictEqual(records[0].rttMs, 60);
    assert.strictEqual(records[0].measuredAtUnixMs, 0);
  });

  await t.test('takes the LATEST entry when several are stored', () => {
    const records = parseWarpKeyHealth(
      proxies({
        'warp-key-1': {
          history: [
            { time: '2026-09-03T03:00:00.000Z', delay: 500 },
            { time: '2026-09-03T04:00:00.000Z', delay: 70 },
          ],
        },
      })
    );

    assert.strictEqual(records[0].rttMs, 70);
  });

  await t.test('returns nothing for a malformed payload instead of throwing', () => {
    // Ответ приходит из чужого процесса; падение здесь превратило бы пробу в источник отказов.
    for (const payload of [null, undefined, {}, { proxies: null }, { proxies: 'nope' }, []]) {
      assert.deepStrictEqual(parseWarpKeyHealth(payload), []);
    }
  });
});
