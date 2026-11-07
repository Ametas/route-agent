import test from 'node:test';
import assert from 'node:assert';
import { config } from '../src/config.js';
import {
  planSelfUpdate,
  selfUpdateHandler,
  UPDATE_CHANNEL_HEADER,
} from '../src/services/system.service.js';

process.env.NODE_ENV = 'test';

/**
 * Канал обновления в SelfUpdate (2026-11-07). Оркестратор переводит узел на nightly (ветка `alpha`,
 * Go-агент) заголовком `x-agent-channel`, прото не меняется. Без заголовка всё обязано остаться байт
 * в байт прежним: по старому тексту оркестратор узнаёт агента, который заголовка не понимает.
 */

interface SelfUpdateResponse {
  success: boolean;
  message: string;
}

const LEGACY_COMMAND =
  'cd /opt/route-agent && git fetch --all --tags --force && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent';

function channelCommand(branch: string): string {
  return `cd /opt/route-agent && git fetch --force --tags origin +refs/heads/${branch}:refs/remotes/origin/${branch} && git checkout -q -f -B ${branch} HEAD && git branch -q --set-upstream-to=origin/${branch} ${branch} && git reset --hard @{u} && git clean -fd && npm ci && npm run build && systemctl restart route-agent`;
}

function callHandler(headers: Record<string, string[]>): Promise<SelfUpdateResponse> {
  return new Promise((resolve, reject) => {
    const call = {
      metadata: { get: (key: string) => headers[key] ?? [] },
      request: {},
    };
    void selfUpdateHandler(call as never, ((err: unknown, res: SelfUpdateResponse) => {
      if (err) reject(err);
      else resolve(res);
    }) as never);
  });
}

test('без заголовка — прежняя команда и прежний ответ', () => {
  const plan = planSelfUpdate(undefined);
  assert.deepStrictEqual(plan, {
    accepted: true,
    message: 'Self-update sequence initiated. Agent will pull code and restart in 1 second.',
    command: LEGACY_COMMAND,
  });
});

test('nightly — ветка alpha, токен channel= в ответе', () => {
  const plan = planSelfUpdate('nightly');
  assert.deepStrictEqual(plan, {
    accepted: true,
    message:
      'Self-update sequence initiated (channel=nightly). Agent will switch to branch alpha and restart in 1 second.',
    command: channelCommand('alpha'),
  });
});

test('stable — ветка main', () => {
  const plan = planSelfUpdate('stable');
  assert.deepStrictEqual(plan, {
    accepted: true,
    message:
      'Self-update sequence initiated (channel=stable). Agent will switch to branch main and restart in 1 second.',
    command: channelCommand('main'),
  });
});

test('неизвестный канал — отказ, команды нет; значение в ответе не длиннее 32 символов', () => {
  assert.deepStrictEqual(planSelfUpdate('Nightly'), {
    accepted: false,
    message: "Unknown update channel 'Nightly'.",
  });
  assert.deepStrictEqual(planSelfUpdate(''), { accepted: false, message: "Unknown update channel ''." });
  // Имена из прототипа объекта каналом не становятся.
  assert.strictEqual(planSelfUpdate('constructor').accepted, false);
  assert.strictEqual(planSelfUpdate('toString').accepted, false);

  const injected = 'main; rm -rf / && echo ' + 'x'.repeat(40);
  const plan = planSelfUpdate(injected);
  assert.strictEqual(plan.accepted, false);
  assert.strictEqual(plan.message, `Unknown update channel '${injected.slice(0, 32)}'.`);
  assert.ok(!('command' in plan));
});

test('обработчик: неверный секрет отвергается раньше канала', async () => {
  const res = await callHandler({ 'x-orchestrator-secret': ['wrong'], [UPDATE_CHANNEL_HEADER]: ['nightly'] });
  assert.deepStrictEqual(res, { success: false, message: 'Invalid orchestrator secret token.' });
});

test('обработчик: неизвестный канал — отказ без запуска обновления', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const res = await callHandler({
    'x-orchestrator-secret': [config.EGRESS_CONTROL_SECRET],
    [UPDATE_CHANNEL_HEADER]: ['beta'],
  });
  assert.deepStrictEqual(res, { success: false, message: "Unknown update channel 'beta'." });
});

test('обработчик: с заголовком nightly отвечает до запуска, ответ несёт channel=nightly', async (t) => {
  // Таймер обновления замокан и не срабатывает: команда на этой машине не исполняется.
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const res = await callHandler({
    'x-orchestrator-secret': [config.EGRESS_CONTROL_SECRET],
    [UPDATE_CHANNEL_HEADER]: ['nightly'],
  });
  assert.strictEqual(res.success, true);
  assert.match(res.message, /\(channel=nightly\)/);
});

test('обработчик: без заголовка ответ прежний', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const res = await callHandler({ 'x-orchestrator-secret': [config.EGRESS_CONTROL_SECRET] });
  assert.deepStrictEqual(res, {
    success: true,
    message: 'Self-update sequence initiated. Agent will pull code and restart in 1 second.',
  });
});
