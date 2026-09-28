import test from 'node:test';
import assert from 'node:assert';
import { awgBoolDirective } from '../src/services/config.service.js';

/**
 * Флаги AWG 3.1 в конфиге сервера (2026-09-28). awg-tools отвергает конфиг целиком на значении,
 * которое не разбирает, поэтому в файл уходит только on/off.
 */
test('on/off и 0/1 становятся строкой конфига', () => {
  assert.strictEqual(awgBoolDirective('RandomTrailers', 'on'), 'RandomTrailers = on\n');
  assert.strictEqual(awgBoolDirective('RandomTrailers', '1'), 'RandomTrailers = on\n');
  assert.strictEqual(awgBoolDirective('DisableCookies', 'OFF'), 'DisableCookies = off\n');
  assert.strictEqual(awgBoolDirective('DisableCookies', '0'), 'DisableCookies = off\n');
});

test('пусто или непонятное значение — директивы нет', () => {
  assert.strictEqual(awgBoolDirective('RandomTrailers', ''), '');
  assert.strictEqual(awgBoolDirective('RandomTrailers', undefined), '');
  assert.strictEqual(awgBoolDirective('RandomTrailers', 'maybe'), '');
  assert.strictEqual(awgBoolDirective('RandomTrailers', 'on\nPostUp = rm -rf /'), '');
});
