import test from 'node:test';
import assert from 'node:assert';
import { isDkmsInstalledForKernel } from '../src/services/meshTunnel.service.js';

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
