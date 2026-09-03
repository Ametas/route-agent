import test from 'node:test';
import assert from 'node:assert';
import { startAndReloadRear } from '../src/services/rearSingbox.service.js';

/**
 * Выбор между reload и restart для тылового инстанса.
 *
 * Проверяется отдельно от gRPC-пайплайна намеренно: все вызовы `systemctl` в обработчике закорочены
 * под NODE_ENV=test, поэтому через провод этот выбор не виден вовсе — мутационный прогон показал,
 * что подмена reload на restart не роняла ни одного теста. `runExec` параметром — тот же приём, что
 * у `ensureSingboxSystemdUnit`.
 */
test('startAndReloadRear reloads instead of restarting', async () => {
  const commands: string[] = [];
  const runExec = async (command: string) => {
    commands.push(command);
    return { stdout: '', stderr: '' };
  };

  await startAndReloadRear(runExec);

  assert.strictEqual(commands.length, 2, commands.join(' | '));
  assert.match(commands[0], /^systemctl enable --now /);

  /**
   * Именно reload. Restart сработал бы тоже, но потерял бы единственную защиту, которую даёт
   * `ExecReload` этого юнита: он прогоняет `sing-box check` и посылает SIGHUP только при успехе,
   * то есть негодный конфиг оставляет тыл работать вместо падения.
   */
  assert.match(commands[1], /^systemctl reload /);
  assert.ok(!commands.some((c) => c.includes('restart')), commands.join(' | '));
});
