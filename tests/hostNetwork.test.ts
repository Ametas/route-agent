import test from 'node:test';
import assert from 'node:assert';
import { defaultRouteInterface, interfaceCounters, hostNetworkFields } from '../src/utils/hostNetwork.js';

/**
 * Трафик машины для плиток оркестратора (2026-10-21): интерфейс маршрута по умолчанию, а не сумма
 * всех — мосты, veth и туннель меша задвоили бы трафик.
 */

const ROUTE = [
  'Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT',
  'awg-mesh\t0000000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0',
  'eth1\t00000000\t0101A8C0\t0003\t0\t0\t200\t00000000\t0\t0\t0',
  'eth0\t00000000\t010010AC\t0003\t0\t0\t100\t00000000\t0\t0\t0',
  'docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0',
].join('\n');

const DEV = [
  'Inter-|   Receive                                                |  Transmit',
  ' face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed',
  '    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0',
  '  eth0: 98765432100 70000 0 0 0 0 0 0 12345678900 60000 0 0 0 0 0 0',
].join('\n');

test('маршрут по умолчанию: назначение 0 и меньшая метрика', () => {
  assert.strictEqual(defaultRouteInterface(ROUTE), 'eth0');
});

test('маршрута по умолчанию нет — null', () => {
  assert.strictEqual(defaultRouteInterface(ROUTE.split('\n').filter((l) => !l.includes('\t00000000\t0')).join('\n')), null);
});

test('счётчики интерфейса: байты приёма и отдачи', () => {
  assert.deepStrictEqual(interfaceCounters(DEV, 'eth0'), { rxBytes: 98765432100, txBytes: 12345678900 });
  assert.strictEqual(interfaceCounters(DEV, 'eth9'), null);
});

test('поля телеметрии: без данных — пустое имя интерфейса', () => {
  assert.deepStrictEqual(hostNetworkFields(null), { hostNetIface: '', hostNetRxBytes: 0, hostNetTxBytes: 0 });
  assert.deepStrictEqual(hostNetworkFields({ iface: 'eth0', rxBytes: 5, txBytes: 7 }), { hostNetIface: 'eth0', hostNetRxBytes: 5, hostNetTxBytes: 7 });
});
