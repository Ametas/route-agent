import test from 'node:test';
import assert from 'node:assert';
import { parseMeshPeerDump } from '../src/utils/telemetry.js';

/**
 * Ключи живых пиров меша (2026-09-26). Оркестратор ведёт трафик кольца от фронта к egress-узлу
 * через туннель, только пока у этой пары свежее рукопожатие, — ключи он берёт отсюда.
 */

const NOW = 1_790_400_000;

// Формат `awg show <iface> dump`: строка интерфейса, дальше по строке на пира, пятое поле — время
// последнего рукопожатия (0 — не было ни разу).
const DUMP = [
  'PRIVKEY=\tPUBKEY=\t51821\toff',
  `live1=\t(none)\t146.19.128.73:51821\t100.100.0.5/32\t${NOW - 27}\t118000\t164000\t25`,
  `never=\t(none)\t195.209.218.173:51821\t100.100.0.3/32\t0\t0\t114000\t25`,
  `stale=\t(none)\t77.221.151.250:51821\t100.100.0.2/32\t${NOW - 181}\t0\t114000\t25`,
  `edge=\t(none)\t2.27.54.246:51821\t100.100.0.4/32\t${NOW - 180}\t10\t20\t25`,
].join('\n');

test('живыми считаются пиры с рукопожатием не старше 180 секунд', () => {
  assert.deepStrictEqual(parseMeshPeerDump(DUMP, NOW), { activePeers: 2, livePeerKeys: ['live1=', 'edge='] });
});

test('интерфейс без пиров — ни одного живого', () => {
  assert.deepStrictEqual(parseMeshPeerDump('PRIVKEY=\tPUBKEY=\t51821\toff\n', NOW), { activePeers: 0, livePeerKeys: [] });
});

test('обрезанная строка пропускается, а не роняет разбор', () => {
  assert.deepStrictEqual(parseMeshPeerDump(`iface\nbroken=\t(none)\n${DUMP.split('\n')[1]}`, NOW), {
    activePeers: 1,
    livePeerKeys: ['live1='],
  });
});
