import test from 'node:test';
import assert from 'node:assert';
import os from 'os';
import { defaultRouteInterface, globalIpv6Addresses, hostIpv6Fields, hostNetworkFields, interfaceCounters, isGlobalUnicastIpv6 } from '../src/utils/hostNetwork.js';

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

// Глобальные IPv6 машины (2026-11-21): по ним оркестратор узнаёт фронт, пришедший через
// Cloudflare по IPv6. Только 2000::/3 — адреса меша (ULA), link-local и loopback не нужны.

const ifc = (address: string, family: 'IPv4' | 'IPv6', internal = false) =>
  ({ address, family, internal, netmask: '', mac: '', cidr: null, scopeid: 0 }) as os.NetworkInterfaceInfo;

test('глобальный unicast IPv6 — только 2000::/3', () => {
  for (const a of ['2a12:5940:e3d1::2', '2001:db8::1', '3fff::1']) assert.equal(isGlobalUnicastIpv6(a), true, a);
  for (const a of ['::1', 'fe80::1%eth0', 'fd00:100::2', 'fc00::1', 'ff02::1', '::ffff:1.2.3.4', '4000::1', '', 'garbage']) {
    assert.equal(isGlobalUnicastIpv6(a), false, a);
  }
});

test('адреса машины: без IPv4, внутренних, ULA и повторов, по порядку', () => {
  const list = globalIpv6Addresses({
    lo: [ifc('127.0.0.1', 'IPv4', true), ifc('::1', 'IPv6', true)],
    eth0: [ifc('203.0.113.7', 'IPv4'), ifc('2A12:5940:E3D1::2', 'IPv6'), ifc('fe80::1', 'IPv6')],
    eth1: [ifc('2001:db8::5', 'IPv6'), ifc('2a12:5940:e3d1::2', 'IPv6')],
    mesh0: [ifc('fd00:100::2', 'IPv6')],
  });
  assert.deepEqual(list, ['2001:db8::5', '2a12:5940:e3d1::2']);
});

test('поля телеметрии: агент знает поле даже без IPv6', () => {
  assert.deepEqual(hostIpv6Fields({ eth0: [ifc('203.0.113.7', 'IPv4')] }), { hostGlobalIpv6: [], hostGlobalIpv6Reported: true });
});
