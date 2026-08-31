import test from 'node:test';
import assert from 'node:assert/strict';
import { computeUserTrafficDeltas } from '../src/utils/singboxStats.js';
import type { SingBoxConnectionRecord } from '../src/utils/singboxConnections.js';

/**
 * Пер-юзерные счётчики поверх Clash API.
 *
 * Оркестратор трактует присланное число как трафик за ОДИН тик свипа и сравнивает с порогом
 * всплеска. Clash API отдаёт абсолютные байты, поэтому приращение считает агент — и почти все
 * способы ошибиться здесь дают не пустой результат, а завышенный, то есть ложный всплеск. Отсюда
 * набор ниже: он проверяет не «работает ли», а «не считает ли лишнего».
 */

function conn(
  id: string,
  user: string,
  uploadBytes: number,
  downloadBytes: number,
  closed = false
): SingBoxConnectionRecord {
  return {
    id,
    user,
    sourceIp: '10.0.0.1',
    destinationIp: '93.184.216.34',
    destinationDomain: 'example.com',
    destinationPort: 443,
    uploadBytes,
    downloadBytes,
    network: 'tcp',
    inboundTag: 'tuic-in',
    startedAtUnixMs: 1_800_000_000_000,
    closed,
  };
}

const EMPTY = new Map<string, { upload: number; download: number }>();

test('the first pass reports nothing and only records the baseline', () => {
  // Соединения, открытые ДО старта агента, приходят с уже накопленными байтами. Посчитать их как
  // приращение за один тик — это отрапортовать гигабайты за три минуты, то есть ложный всплеск
  // при каждом перезапуске агента.
  const { deltas, next } = computeUserTrafficDeltas(
    [conn('c1', 'user-a', 5_000_000_000, 9_000_000_000)],
    EMPTY,
    true
  );

  assert.deepEqual(deltas, []);
  assert.deepEqual(next.get('c1'), { upload: 5_000_000_000, download: 9_000_000_000 });
});

test('a growing connection reports only the increment, not the running total', () => {
  const first = computeUserTrafficDeltas([conn('c1', 'user-a', 1000, 2000)], EMPTY, true);
  const second = computeUserTrafficDeltas([conn('c1', 'user-a', 1500, 2600)], first.next, false);

  assert.deepEqual(second.deltas, [{ userUuid: 'user-a', uplinkBytes: 500, downlinkBytes: 600 }]);
});

test('a brand new connection contributes its full byte count', () => {
  const first = computeUserTrafficDeltas([conn('c1', 'user-a', 1000, 1000)], EMPTY, true);
  const second = computeUserTrafficDeltas(
    [conn('c1', 'user-a', 1000, 1000), conn('c2', 'user-a', 700, 300)],
    first.next,
    false
  );

  // c1 не вырос, c2 появился целиком — приращение это ровно байты c2.
  assert.deepEqual(second.deltas, [{ userUuid: 'user-a', uplinkBytes: 700, downlinkBytes: 300 }]);
});

test('a connection lingering in the closed buffer is not counted twice', () => {
  // Главная ловушка: Clash отдаёт буфер завершённых несколько проходов подряд. Если забывать
  // закрытое сразу после подсчёта, на следующем проходе оно выглядит новым — и его полные байты
  // уезжают в оркестратор ещё раз, и ещё, пока не вытеснится из буфера.
  const first = computeUserTrafficDeltas([conn('c1', 'user-a', 400, 100)], EMPTY, true);
  const second = computeUserTrafficDeltas([conn('c1', 'user-a', 900, 250, true)], first.next, false);
  const third = computeUserTrafficDeltas([conn('c1', 'user-a', 900, 250, true)], second.next, false);

  assert.deepEqual(second.deltas, [{ userUuid: 'user-a', uplinkBytes: 500, downlinkBytes: 150 }]);
  assert.deepEqual(third.deltas, [], 'то же завершённое соединение во втором проходе — уже посчитано');
});

test('a connection that aged out of both sets is released', () => {
  const first = computeUserTrafficDeltas([conn('c1', 'user-a', 400, 100)], EMPTY, true);
  const second = computeUserTrafficDeltas([], first.next, false);

  assert.deepEqual(second.deltas, []);
  assert.equal(second.next.size, 0, 'то, чего не было в проходе, не переносится — иначе карта росла бы вечно');
});

test('bytes from several connections land on the right users', () => {
  const first = computeUserTrafficDeltas(
    [conn('c1', 'user-a', 0, 0), conn('c2', 'user-a', 0, 0), conn('c3', 'user-b', 0, 0)],
    EMPTY,
    true
  );
  const second = computeUserTrafficDeltas(
    [conn('c1', 'user-a', 100, 10), conn('c2', 'user-a', 200, 20), conn('c3', 'user-b', 50, 5)],
    first.next,
    false
  );

  const byUser = Object.fromEntries(second.deltas.map((d) => [d.userUuid, d]));
  assert.deepEqual(byUser['user-a'], { userUuid: 'user-a', uplinkBytes: 300, downlinkBytes: 30 });
  assert.deepEqual(byUser['user-b'], { userUuid: 'user-b', uplinkBytes: 50, downlinkBytes: 5 });
});

test('a counter that moved backwards yields zero, never a negative delta', () => {
  // Не должно случаться, но отрицательное число оркестратор вычтет из дневного накопителя —
  // то есть одна кривая запись уменьшила бы уже посчитанное за сегодня.
  const first = computeUserTrafficDeltas([conn('c1', 'user-a', 5000, 5000)], EMPTY, true);
  const second = computeUserTrafficDeltas([conn('c1', 'user-a', 100, 100)], first.next, false);

  assert.deepEqual(second.deltas, []);
});

test('records without a user or an id are dropped, not counted from zero', () => {
  // Считать такие с нуля значило бы каждый проход заново записывать всю историю соединения как
  // приращение — то есть ровно тот ложный всплеск, от которого защищает весь остальной модуль.
  const { deltas, next } = computeUserTrafficDeltas(
    [conn('', 'user-a', 900, 900), conn('c2', '', 900, 900)],
    EMPTY,
    false
  );

  assert.deepEqual(deltas, []);
  assert.equal(next.size, 0);
});
