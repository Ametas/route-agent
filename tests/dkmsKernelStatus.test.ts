import test from 'node:test';
import assert from 'node:assert';
import { isDkmsInstalledForKernel, dkmsKnownVersions, patchCompatForKernelHeaders } from '../src/services/meshTunnel.service.js';

/**
 * Модуль меша считается установленным только под работающее ядро (2026-09-28). До правки хватало
 * слова «installed» под любым ядром, и после обновления ядра сборка пропускалась.
 */

const RUNNING = '6.8.0-139-generic';

test('установлен под старое ядро, под работающее нет: надо собирать', () => {
  const out = 'amneziawg/1.0.20250901, 6.8.0-100-generic, x86_64: installed\n';
  assert.strictEqual(isDkmsInstalledForKernel(out, RUNNING), false);
});

test('установлен под работающее ядро: сборку пропускаем (оба формата dkms)', () => {
  assert.strictEqual(isDkmsInstalledForKernel(`amneziawg/1.0.20250901, ${RUNNING}, x86_64: installed\n`, RUNNING), true);
  assert.strictEqual(isDkmsInstalledForKernel(`amneziawg, 1.0.20250901, ${RUNNING}, x86_64: installed\n`, RUNNING), true);
});

test('под работающим ядром только собран или только добавлен: надо ставить', () => {
  assert.strictEqual(isDkmsInstalledForKernel(`amneziawg/1.0.20250901, ${RUNNING}, x86_64: built\n`, RUNNING), false);
  assert.strictEqual(isDkmsInstalledForKernel('amneziawg/1.0.20250901: added\n', RUNNING), false);
  assert.strictEqual(isDkmsInstalledForKernel('', RUNNING), false);
});

test('несколько ядер: находит строку работающего среди прочих', () => {
  const out = [
    'amneziawg/1.0.20250901, 6.8.0-100-generic, x86_64: installed',
    `amneziawg/1.0.20250901, ${RUNNING}, x86_64: installed`,
  ].join('\n');
  assert.strictEqual(isDkmsInstalledForKernel(out, RUNNING), true);
});

test('похожее имя ядра не считается совпадением', () => {
  assert.strictEqual(isDkmsInstalledForKernel('amneziawg/1.0, 6.8.0-139-generic-64k, x86_64: installed', RUNNING), false);
});

/**
 * Прежние деревья модуля снимаются до `apt-get install` (2026-10-18): autoinstall dkms при настройке
 * `linux-headers` нового ядра падал на старом дереве и валил установку зависимостей целиком.
 */
test('dkmsKnownVersions: все версии модуля в обоих форматах и в коротком без ядра', () => {
  const out = [
    'amneziawg/1.0.0, 7.0.0-22-generic, x86_64: installed',
    'amneziawg/1.0.0: added',
    'amneziawg, 1.0.20250901, 7.0.0-38-generic, x86_64: built',
    'other/2.0, 7.0.0-38-generic, x86_64: installed',
    '',
  ].join('\n');
  assert.deepStrictEqual(dkmsKnownVersions(out, 'amneziawg'), ['1.0.0', '1.0.20250901']);
  assert.deepStrictEqual(dkmsKnownVersions('', 'amneziawg'), []);
});

/**
 * Совместимость по заголовкам работающего ядра (2026-10-18). compat.h amneziawg решал по номеру
 * версии (< 7.1.5), а Ubuntu 7.0.0-38 уже несёт новую сигнатуру со `struct sock *` — сборка падала
 * на «incompatible pointer type».
 */
const COMPAT = [
  '#if LINUX_VERSION_CODE < KERNEL_VERSION(7, 1, 5)',
  '#include <net/udp_tunnel.h>',
  '#define setup_udp_tunnel_sock(net, sk, sock_cfg) setup_udp_tunnel_sock(net, sk->sk_socket, sock_cfg)',
  '#define udp_tunnel_sock_release(sk) udp_tunnel_sock_release(sk->sk_socket)',
  '#endif',
  '',
].join('\n');

test('новая сигнатура в заголовке ядра: макросы совместимости снимаются', () => {
  const header = [
    'void setup_udp_tunnel_sock(struct net *net, struct sock *sk,',
    '\t\t\t   struct udp_tunnel_sock_cfg *sock_cfg);',
    'void udp_tunnel_sock_release(struct sock *sk);',
  ].join('\n');
  const { source, dropped } = patchCompatForKernelHeaders(COMPAT, header);
  assert.deepStrictEqual(dropped, ['setup_udp_tunnel_sock', 'udp_tunnel_sock_release']);
  assert.ok(!source.includes('sk->sk_socket'));
  assert.ok(source.includes('#include <net/udp_tunnel.h>'));
  assert.ok(source.includes('#endif'));
});

test('старая сигнатура (struct socket *): исходник не трогается', () => {
  const header = [
    'void setup_udp_tunnel_sock(struct net *net, struct socket *sock,',
    '\t\t\t   struct udp_tunnel_sock_cfg *sock_cfg);',
    'void udp_tunnel_sock_release(struct socket *sock);',
  ].join('\n');
  const { source, dropped } = patchCompatForKernelHeaders(COMPAT, header);
  assert.deepStrictEqual(dropped, []);
  assert.strictEqual(source, COMPAT);
});

test('перенесена только одна функция: снимается только её макрос', () => {
  const header = 'void setup_udp_tunnel_sock(struct net *net, struct sock *sk, struct udp_tunnel_sock_cfg *cfg);\nvoid udp_tunnel_sock_release(struct socket *sock);';
  const { source, dropped } = patchCompatForKernelHeaders(COMPAT, header);
  assert.deepStrictEqual(dropped, ['setup_udp_tunnel_sock']);
  assert.ok(source.includes('udp_tunnel_sock_release(sk->sk_socket)'));
});
