import test from 'node:test';
import assert from 'node:assert';
import { meshAwg31Directives } from '../src/services/meshTunnel.service.js';

/**
 * Поля AWG 3.1 в конфиге меша (2026-11-14): число или диапазон — пишется, прочее — нет; флаги —
 * on/off, пусто — директивы нет. Старый `awg setconf` отверг бы незнакомый ключ, поэтому их
 * присылает только оркестратор, знающий, что на узле 3.1.
 */

test('таймеры и добивка — числом и диапазоном, флаги — on/off', () => {
  assert.strictEqual(
    meshAwg31Directives({ contentPaddingAddition: '24-34', rekeyAfterTime: '104-118', rekeyTimeout: '4-5', rejectAfterTime: '170-195', keepaliveTimeout: '12', maxHandshakeAttempts: '16-20', randomTrailers: 'on', disableCookies: 'off' }),
    'ContentPaddingAddition = 24-34\nRekeyAfterTime = 104-118\nRekeyTimeout = 4-5\nRejectAfterTime = 170-195\nKeepaliveTimeout = 12\nMaxHandshakeAttempts = 16-20\nRandomTrailers = on\nDisableCookies = off\n'
  );
});

test('ничего не прислано — ничего не пишется (умолчания модуля)', () => {
  assert.strictEqual(meshAwg31Directives({}), '');
});

test('мусор и попытка дописать строку не попадают в конфиг', () => {
  assert.strictEqual(meshAwg31Directives({ rekeyTimeout: '5\nPostUp = rm -rf /', keepaliveTimeout: 'abc', randomTrailers: 'maybe' }), '');
});
