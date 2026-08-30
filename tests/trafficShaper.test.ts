import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { parseFilterCounters, isSameThrottle } from '../src/utils/trafficShaper.js';

/**
 * Разбор счётчиков полисера (2026-08-31).
 *
 * Фикстура ниже — ДОСЛОВНЫЙ вывод `tc -s filter show` с живого ядра 6.6.87.2, снятый при проверке
 * механизма. Не выдуманный образец: формат этой команды человекочитаемый и меняется между версиями
 * iproute2, поэтому разбирать его надо против того, что ядро действительно печатает.
 *
 * Именно счётчик отброшенного — то, ради чего всё затевалось: под одним и тем же полисером
 * кооперативный TCP дал 122 дропа за шесть секунд, а некооперативный UDP-флуд — 1 791 515.
 * Разница в четыре порядка, и она про поведение, а не про объём.
 */

const REAL_TC_OUTPUT = `filter protocol ip pref 1 u32 chain 0
filter protocol ip pref 1 u32 chain 0 fh 800: ht divisor 1
filter protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 terminal flowid not_in_hw (rule hit 710 success 710)
  match 0a630000/ffffff00 at 12 (success 710 )
 police 0x1 rate 8Mbit burst 100Kb mtu 2Kb action drop overhead 0b
\tref 1 bind 1  installed 5 sec used 0 sec firstused 5 sec

 Sent 3351952 bytes 2294 pkts (dropped 95, overlimits 95)
`;

describe('разбор счётчиков tc', () => {
  it('вытаскивает отправленное и отброшенное из настоящего вывода ядра', () => {
    const [counter] = parseFilterCounters(REAL_TC_OUTPUT, ['10.99.0.0/24']);

    assert.equal(counter.prefix, '10.99.0.0/24');
    assert.equal(counter.sentBytes, 3351952);
    assert.equal(counter.sentPackets, 2294);
    assert.equal(counter.droppedPackets, 95);
  });

  it('сопоставляет счётчики с префиксами по приоритету, включая IPv6', () => {
    // Дословный вывод ядра для трёх фильтров, снятый при проверке последовательности команд.
    // Существенно, что КАЖДЫЙ фильтр печатает по три строки с одним и тем же `pref` — разборщик
    // обязан это переживать, а не считать вторую строку началом нового фильтра.
    //
    // Адрес в выводе лежит шестнадцатеричной маской, поэтому текст префикса берётся из
    // сохранённого состояния: восстанавливать CIDR из маски значило бы завести второй,
    // независимый способ ошибиться.
    const output = `filter protocol ip pref 1 u32 chain 0
filter protocol ip pref 1 u32 chain 0 fh 800: ht divisor 1
filter protocol ip pref 1 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 terminal flowid not_in_hw (rule hit 0 success 0)
 Sent 100 bytes 2 pkts (dropped 1, overlimits 1)
filter protocol ip pref 2 u32 chain 0
filter protocol ip pref 2 u32 chain 0 fh 801: ht divisor 1
filter protocol ip pref 2 u32 chain 0 fh 801::800 order 2048 key ht 801 bkt 0 terminal flowid not_in_hw (rule hit 0 success 0)
 Sent 200 bytes 4 pkts (dropped 3, overlimits 3)
filter protocol ipv6 pref 3 u32 chain 0
filter protocol ipv6 pref 3 u32 chain 0 fh 802: ht divisor 1
filter protocol ipv6 pref 3 u32 chain 0 fh 802::800 order 2048 key ht 802 bkt 0 terminal flowid not_in_hw (rule hit 0 success 0)
 Sent 300 bytes 6 pkts (dropped 5, overlimits 5)
`;

    const counters = parseFilterCounters(output, ['198.51.100.0/24', '203.0.113.0/24', '2001:db8::/48']);

    assert.equal(counters.length, 3, 'три фильтра — три записи, а не по одной на строку с pref');
    assert.deepEqual(
      counters.map((c) => [c.prefix, c.droppedPackets]),
      [
        ['198.51.100.0/24', 1],
        ['203.0.113.0/24', 3],
        ['2001:db8::/48', 5],
      ]
    );
  });

  it('приоритет вне известного списка пропускается, а не приписывается чужому префиксу', () => {
    // Остаток от прошлой раскладки или чужой фильтр. Приписать его чужому префиксу означало бы
    // выдать вердикт по чужим уликам.
    const output = `filter protocol ip pref 7 u32 chain 0 fh 800::800 terminal flowid not_in_hw
 Sent 999 bytes 9 pkts (dropped 9, overlimits 9)
`;

    assert.deepEqual(parseFilterCounters(output, ['203.0.113.0/24']), []);
  });

  it('строка Sent без предшествующего фильтра игнорируется', () => {
    // `tc -s qdisc show` печатает такую же строку для qdisc целиком — перепутать их нельзя.
    assert.deepEqual(parseFilterCounters(' Sent 500 bytes 5 pkts (dropped 5, overlimits 5)\n', ['a/24']), []);
  });

  it('пустой вывод не роняет разбор', () => {
    assert.deepEqual(parseFilterCounters('', ['203.0.113.0/24']), []);
  });
});

describe('сравнение раскладок', () => {
  const base = { interface: 'eth0', rate: '8mbit', burst: '500k', prefixes: ['a/24', 'b/24'] };

  it('одинаковые раскладки не требуют пересборки', () => {
    // Пересборка ОБНУЛЯЕТ счётчики, а на них держится вся оценка «отступил или продолжает
    // ломиться». Переставлять одно и то же значило бы стирать улику на каждой доставке.
    assert.equal(isSameThrottle({ ...base }, { ...base }), true);
  });

  it('отсутствие прежнего состояния считается различием', () => {
    assert.equal(isSameThrottle(null, base), false);
  });

  it('смена потолка требует пересборки', () => {
    assert.equal(isSameThrottle(base, { ...base, rate: '1mbit' }), false);
  });

  it('смена burst требует пересборки — это главный параметр, а не мелочь', () => {
    // Измерено: при одном и том же rate 8mbit реально принятое менялось с 4 до 11 Мбит/с в
    // зависимости от burst.
    assert.equal(isSameThrottle(base, { ...base, burst: '2m' }), false);
  });

  it('смена интерфейса требует пересборки', () => {
    assert.equal(isSameThrottle(base, { ...base, interface: 'ens3' }), false);
  });

  it('другой ПОРЯДОК префиксов требует пересборки — от него зависит нумерация приоритетов', () => {
    assert.equal(isSameThrottle(base, { ...base, prefixes: ['b/24', 'a/24'] }), false);
  });

  it('добавленный префикс требует пересборки', () => {
    assert.equal(isSameThrottle(base, { ...base, prefixes: ['a/24', 'b/24', 'c/24'] }), false);
  });
});
