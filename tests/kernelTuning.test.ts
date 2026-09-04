import test from 'node:test';
import assert from 'node:assert';
import * as fs from 'fs/promises';
import os from 'os';
import path from 'path';
import { config } from '../src/config.js';
import {
  KERNEL_TUNING,
  applyKernelTuning,
  renderSysctlFile,
  type KernelTuningEntry,
} from '../src/utils/kernelTuning.js';

process.env.NODE_ENV = 'test';

/**
 * Профиль сетевых буферов ядра.
 *
 * Проверяется через подставной `execFile` — тот же приём, что у `startAndReloadRear`: настоящий
 * `sysctl` на машине разработчика ни поднять нельзя, ни осмысленно проверить, а решается здесь не
 * «умеет ли ядро», а «по каким ключам мы идём и что делаем с отказом».
 */

interface Call {
  file: string;
  args: string[];
}

function fakeExecFile(behaviour: (call: Call) => string | Error = () => '') {
  const calls: Call[] = [];
  const run = (async (file: string, args: string[]) => {
    calls.push({ file, args });
    const outcome = behaviour({ file, args });
    if (outcome instanceof Error) throw outcome;
    return { stdout: outcome, stderr: '' };
  }) as never;
  return { calls, run };
}

async function withTempProfile<T>(body: (profilePath: string) => Promise<T>): Promise<T> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'sysctl-profile-'));
  const original = config.SYSCTL_PROFILE_PATH;
  config.SYSCTL_PROFILE_PATH = path.join(dir, '99-route-agent.conf');
  try {
    return await body(config.SYSCTL_PROFILE_PATH);
  } finally {
    config.SYSCTL_PROFILE_PATH = original;
    await fs.rm(dir, { recursive: true, force: true });
  }
}

test('the profile raises the QUIC receive buffer ceiling and does not pre-double it', () => {
  /**
   * Ядро хранит удвоенное значение и `getsockopt` вернёт 15000000 при запрошенных 7,5 МБ — но
   * потолок сравнивается с ЗАПРОШЕННЫМ числом. Проверено на ядре 6.6: при `rmem_max=212992` сокет,
   * попросивший 7500000, получает 212992; при `rmem_max=7500000` — все 7500000.
   *
   * Тест держит именно это: удвоить величину «чтобы наверняка» значило бы отдать вдвое больше
   * памяти на сокет без всякой пользы.
   */
  const rmem = KERNEL_TUNING.find((e) => e.key === 'net.core.rmem_max');
  const wmem = KERNEL_TUNING.find((e) => e.key === 'net.core.wmem_max');

  assert.strictEqual(rmem?.value, '7500000');
  assert.strictEqual(wmem?.value, '7500000');
});

test('the profile does not touch values the kernel derives from RAM', () => {
  /**
   * `udp_mem`, `tcp_rmem`, `tcp_wmem` считаются от объёма памяти при загрузке, и на ноде с другим
   * объёмом наше зашитое число было бы хуже родного умолчания. `rmem_default` не трогаем по другой
   * причине: он раздаёт буфер КАЖДОМУ сокету, включая тысячи короткоживущих, — расход памяти без
   * выигрыша, потому что quic-go просит явно и ему достаточно потолка.
   */
  const keys = KERNEL_TUNING.map((e) => e.key);

  for (const forbidden of [
    'net.ipv4.udp_mem',
    'net.ipv4.tcp_rmem',
    'net.ipv4.tcp_wmem',
    'net.core.rmem_default',
    'net.core.wmem_default',
  ]) {
    assert.ok(!keys.includes(forbidden), `${forbidden} попал в профиль`);
  }
});

test('every key is written to the file with its reason', () => {
  const rendered = renderSysctlFile();

  for (const entry of KERNEL_TUNING) {
    assert.ok(rendered.includes(`${entry.key} = ${entry.value}`), `нет строки для ${entry.key}`);
    assert.ok(rendered.includes(entry.why), `нет объяснения для ${entry.key}`);
  }
});

test('every key is applied and read back from the kernel', async () => {
  await withTempProfile(async (profilePath) => {
    const { calls, run } = fakeExecFile(({ args }) => (args[0] === '-n' ? '7500000\n' : ''));

    const result = await applyKernelTuning(run);

    for (const entry of KERNEL_TUNING) {
      assert.ok(
        calls.some((c) => c.file === 'sysctl' && c.args[0] === '-w' && c.args[1] === `${entry.key}=${entry.value}`),
        `${entry.key} не применялся`
      );
      assert.ok(
        calls.some((c) => c.args[0] === '-n' && c.args[1] === entry.key),
        `${entry.key} не перечитан после записи`
      );
    }

    assert.ok(result.keys.every((k) => k.applied));
    assert.strictEqual(result.fileChanged, true);
    assert.ok((await fs.readFile(profilePath, 'utf-8')).includes('net.core.rmem_max = 7500000'));
  });
});

test('the effective value comes from the kernel, not from our intent', async () => {
  /**
   * Ядро может принять команду и округлить величину. Докладывать при этом задуманное число значило
   * бы отчитываться о намерении — а телеграм-логи должны показывать то, что на ноде на самом деле.
   */
  await withTempProfile(async () => {
    const entries: KernelTuningEntry[] = [{ key: 'net.core.rmem_max', value: '7500000', why: 'w' }];
    const { run } = fakeExecFile(({ args }) => (args[0] === '-n' ? '7499776\n' : ''));

    const result = await applyKernelTuning(run, entries);

    assert.strictEqual(result.keys[0].effective, '7499776');
  });
});

test('a read-only key is reported without stopping the rest', async () => {
  /**
   * На OpenVZ и в контейнерах часть ключей доступна только для чтения. Один такой не должен
   * уносить с собой остальные, и уж точно не должен мешать агенту стартовать: агент, не поднявшийся
   * из-за sysctl, хуже агента с неоптимальными буферами.
   */
  await withTempProfile(async () => {
    const entries: KernelTuningEntry[] = [
      { key: 'net.core.rmem_max', value: '7500000', why: 'a' },
      { key: 'net.core.netdev_max_backlog', value: '8192', why: 'b' },
    ];
    const { calls, run } = fakeExecFile(({ args }) => {
      if (args[0] === '-w' && args[1]?.startsWith('net.core.rmem_max')) {
        return new Error('sysctl: permission denied on key "net.core.rmem_max"');
      }
      return args[0] === '-n' ? '8192\n' : '';
    });

    const result = await applyKernelTuning(run, entries);

    assert.strictEqual(result.keys[0].applied, false);
    assert.match(String(result.keys[0].error), /permission denied/);
    assert.strictEqual(result.keys[1].applied, true, 'отказ первого ключа унёс второй');
    assert.ok(
      calls.some((c) => c.args[1] === 'net.core.netdev_max_backlog=8192'),
      'второй ключ не применялся после отказа первого'
    );
  });
});

test('a failed read-back does not count as a failed apply', async () => {
  await withTempProfile(async () => {
    const entries: KernelTuningEntry[] = [{ key: 'net.core.rmem_max', value: '7500000', why: 'w' }];
    const { run } = fakeExecFile(({ args }) =>
      args[0] === '-n' ? new Error('sysctl: cannot stat /proc/sys/net/core/rmem_max') : ''
    );

    const result = await applyKernelTuning(run, entries);

    assert.strictEqual(result.keys[0].applied, true, 'запись прошла, но применение сочли неудачным');
    assert.strictEqual(result.keys[0].effective, null);
  });
});

test('the sysctl file is rewritten only when it differs', async () => {
  /**
   * Профиль применяется при КАЖДОМ старте агента, а агент перезапускается на каждом самообновлении
   * флота. Перезапись без изменений — это лишняя запись в /etc на каждый рестарт.
   */
  await withTempProfile(async () => {
    const { run } = fakeExecFile(({ args }) => (args[0] === '-n' ? '7500000\n' : ''));

    assert.strictEqual((await applyKernelTuning(run)).fileChanged, true);
    assert.strictEqual((await applyKernelTuning(run)).fileChanged, false, 'файл перезаписан без изменений');
  });
});
