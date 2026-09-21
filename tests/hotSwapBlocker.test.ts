import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { describeConfigDiff, explainHotSwapBlocker, planRosterUpdate } from '../src/utils/hotUsers.js';

/**
 * ПОЧЕМУ ГОРЯЧАЯ ЗАМЕНА НЕ ПРОШЛА — ОТКАЗ ОБЯЗАН НАЗЫВАТЬ ПРИЧИНУ (2026-09-21).
 *
 * `planRosterUpdate` отвечает одним `null` на любую неясность, и до этого дня отказ был
 * полностью немым: узел с форковым ядром уходил в полную перезагрузку, рвал сессии всем
 * абонентам и не оставлял в журнале ни строчки. Обнаружилось это случайно — владелец снял
 * счётчик живых соединений до и после создания абонента и увидел 254 → 0 при живом форке.
 *
 * Поэтому здесь проверяется не «диф найден», а два свойства объяснения: оно указывает МЕСТО и
 * оно не печатает ЗНАЧЕНИЯ. Второе не про аккуратность — в конфиге лежат приватные ключи reality,
 * пароли инбаундов и сертификаты, и лог с ними стал бы хранилищем секретов.
 */

const SECRET = 'PRIVATE-KEY-DO-NOT-LOG';

function config(overrides: Record<string, unknown> = {}): object {
  return {
    log: { level: 'warn' },
    inbounds: [
      { type: 'vless', tag: 'vless-in', listen: '::', users: [{ name: 'a', uuid: 'uuid-a' }] },
      { type: 'tuic', tag: 'tuic-in', listen: '::', users: [{ name: 'a', uuid: 'uuid-a', password: SECRET }] },
    ],
    outbounds: [{ type: 'direct', tag: 'direct' }],
    ...overrides,
  };
}

describe('объяснение отказа горячей замены', () => {
  it('различие только в наборах абонентов блокировщиком не является', () => {
    const next = config();
    (next as { inbounds: { users: unknown[] }[] }).inbounds[0]!.users.push({ name: 'b', uuid: 'uuid-b' });

    assert.notEqual(planRosterUpdate(next, config()), null, 'план не построился на чистом дифе наборов');
  });

  it('называет ПУТЬ до различия вне наборов', () => {
    const next = config();
    (next as { inbounds: { listen: string }[] }).inbounds[1]!.listen = '127.0.0.1';

    const blocker = explainHotSwapBlocker(next, config());

    assert.match(blocker, /вне наборов абонентов/);
    assert.match(blocker, /inbounds\[1\]\.listen/, blocker);
  });

  it('ловит смену порядка ключей — её не видно «по значениям»', () => {
    /**
     * Сравнение идёт через `JSON.stringify`, для которого `{a,b}` и `{b,a}` — разные строки.
     * Диф, сравнивающий только значения, показал бы «всё одинаково», и причина выглядела бы
     * необъяснимой.
     */
    const current = { inbounds: [], log: { level: 'warn' }, outbounds: [] };
    const next = { log: { level: 'warn' }, inbounds: [], outbounds: [] };

    assert.match(explainHotSwapBlocker(next, current), /порядок ключей/);
  });

  it('называет добавленный и убранный ключ', () => {
    const next = config({ services: [{ type: 'users-api' }] });

    assert.match(explainHotSwapBlocker(next, config()), /добавлено: services/);
  });

  it('НИКОГДА не печатает значения — только пути', () => {
    /**
     * Самая важная проверка файла. Пароль инбаунда, приватный ключ reality и сертификат лежат в
     * том же объекте, и объяснение, печатающее «было X, стало Y», унесло бы их в журнал.
     */
    const next = config();
    (next as { inbounds: { users: { password?: string }[] }[] }).inbounds[1]!.users[0]!.password = 'NEW-SECRET';
    (next as { outbounds: { tag: string }[] }).outbounds[0]!.tag = 'renamed';

    const blocker = explainHotSwapBlocker(next, config());

    assert.ok(!blocker.includes(SECRET), blocker);
    assert.ok(!blocker.includes('NEW-SECRET'), blocker);
    assert.ok(!blocker.includes('renamed'), blocker);
    assert.match(blocker, /outbounds\[0\]\.tag/, blocker);
  });

  it('отсутствие inbounds называется отдельно, а не как «разошлось»', () => {
    assert.match(explainHotSwapBlocker({ log: {} }, config()), /в присланном конфиге нет inbounds/);
    assert.match(explainHotSwapBlocker(config(), { log: {} }), /в конфиге на диске нет inbounds/);
  });

  it('инбаунд, не умеющий горячую замену, назван по типу', () => {
    const current = { inbounds: [{ type: 'shadowsocks', tag: 'ss-in', users: [] }] };
    const next = { inbounds: [{ type: 'shadowsocks', tag: 'ss-in', users: [{ name: 'a' }] }] };

    assert.match(explainHotSwapBlocker(next, current), /shadowsocks не умеет горячую замену/);
  });

  it('одинаковые конфиги различий не имеют', () => {
    assert.equal(describeConfigDiff(config(), config()), null);
  });

  it('разная длина массива называется длиной, а не элементом', () => {
    const diff = describeConfigDiff({ a: [1, 2] }, { a: [1] });

    assert.match(diff ?? '', /длина 1 → 2/);
  });
});
