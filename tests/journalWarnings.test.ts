import test from 'node:test';
import assert from 'node:assert';
import {
  WARNING_PATTERNS,
  aggregateWarnings,
  buildGrepPattern,
  classifyLine,
  collectJournalWarnings,
  parseJournalJson,
  sinceArgument,
} from '../src/utils/journalWarnings.js';

process.env.NODE_ENV = 'test';

/**
 * Сбор предупреждений о тихой деградации из журналов ноды.
 *
 * Класс проблемы: всё работает, ничего не падает, а работает хуже, чем должно, и увидеть это можно
 * только зайдя на ноду. Живой пример — quic-go просит буфер приёма 7,5 МБ, ядро срезает до 208 КБ и
 * печатает ОДНУ строку при подъёме сокета.
 */

/** Дословно из исходников quic-go — не переписанная от руки похожая строка. */
const QUIC_RECV_LINE =
  'failed to sufficiently increase receive buffer size (was: 208 kiB, wanted: 7500 kiB, got: 416 kiB)';

function journalLine(fields: Record<string, unknown>): string {
  return JSON.stringify(fields);
}

test('the grep pattern carries every registered class', () => {
  /**
   * Отбор в journalctl и классификация у нас собираются из ОДНОГО списка. Разойдись они — и класс,
   * добавленный в реестр, тихо не искался бы в журнале вовсе.
   */
  const pattern = buildGrepPattern();

  for (const entry of WARNING_PATTERNS) {
    assert.ok(pattern.includes(entry.regex), `образец ${entry.kind} не попал в --grep`);
  }
});

test('the real quic-go line is classified as a receive-buffer warning', () => {
  assert.strictEqual(classifyLine(QUIC_RECV_LINE), 'quic_recv_buffer');
});

test('"failed to increase" and "failed to sufficiently increase" are different classes', () => {
  /**
   * Это два разных отказа одного места: во втором буфер вырос, но не до нужного, в первом не вырос
   * вовсе. Слить их значило бы потерять разницу между «настроить ядро» и «разбираться, почему
   * setsockopt отказал».
   */
  assert.strictEqual(classifyLine('failed to increase receive buffer size (wanted: 7500 kiB, got 208 kiB)'), 'quic_buffer_not_increased');
  assert.strictEqual(classifyLine(QUIC_RECV_LINE), 'quic_recv_buffer');
});

test('an unrelated journal line matches nothing', () => {
  assert.strictEqual(classifyLine('inbound/hysteria2[hy2-in]: new connection from 1.2.3.4'), null);
});

test('classification ignores letter case', () => {
  /** journalctl зовётся с --case-sensitive=false, разбор не должен быть строже отбора. */
  assert.strictEqual(classifyLine('NF_CONNTRACK: TABLE FULL, dropping packet'), 'conntrack_table_full');
});

test('the --since argument is absolute and counts back from now', () => {
  /**
   * Относительные формы (`-24h`) systemd понимает по-разному от версии к версии; абсолютная
   * читается однозначно и проверяема.
   */
  const now = new Date(2026, 8, 4, 12, 30, 15);

  assert.strictEqual(sinceArgument(24, now), '2026-09-03 12:30:15');
  assert.strictEqual(sinceArgument(1, now), '2026-09-04 11:30:15');
});

test('journalctl JSON is parsed into classified entries', () => {
  const stdout = [
    journalLine({ MESSAGE: QUIC_RECV_LINE, _SYSTEMD_UNIT: 'sing-box.service', __REALTIME_TIMESTAMP: '1788000000000000' }),
    journalLine({ MESSAGE: 'nothing interesting here', _SYSTEMD_UNIT: 'sing-box.service' }),
  ].join('\n');

  const parsed = parseJournalJson(stdout, 'unknown');

  assert.strictEqual(parsed.length, 1, 'непрофильная строка попала в разбор');
  assert.strictEqual(parsed[0].kind, 'quic_recv_buffer');
  assert.strictEqual(parsed[0].source, 'sing-box', 'суффикс .service не снят');
  assert.strictEqual(parsed[0].atUnixMs, 1788000000000, 'микросекунды не переведены в миллисекунды');
});

test('a kernel entry without a unit falls back to the pass source', () => {
  const stdout = journalLine({ MESSAGE: 'nf_conntrack: table full, dropping packet', __REALTIME_TIMESTAMP: '1788000000000000' });

  const parsed = parseJournalJson(stdout, 'kernel');

  assert.strictEqual(parsed[0].source, 'kernel');
});

test('a non-string MESSAGE and a malformed line are skipped, not fatal', () => {
  /**
   * `MESSAGE` бывает массивом байтов, когда в строке невалидный UTF-8. Гадать о кодировке здесь
   * незачем — диагноз в такой строке всё равно не прочитать; важно не уронить весь проход.
   */
  const stdout = [
    journalLine({ MESSAGE: [102, 97, 105, 108], _SYSTEMD_UNIT: 'sing-box.service' }),
    '{ this is not json',
    journalLine({ MESSAGE: QUIC_RECV_LINE, _SYSTEMD_UNIT: 'caddy.service', __REALTIME_TIMESTAMP: '1788000000000000' }),
  ].join('\n');

  const parsed = parseJournalJson(stdout, 'unknown');

  assert.strictEqual(parsed.length, 1);
  assert.strictEqual(parsed[0].source, 'caddy');
});

test('the same class on the front and the rear instance stays two facts', () => {
  /**
   * Ключ свёртки — пара «класс + источник». Срезанный буфер у фронтового sing-box и у тылового это
   * две разные новости, и слить их значило бы половину потерять.
   */
  const warnings = aggregateWarnings([
    { kind: 'quic_recv_buffer', source: 'sing-box', sample: 'a', atUnixMs: 1000 },
    { kind: 'quic_recv_buffer', source: 'route-rear-singbox', sample: 'b', atUnixMs: 2000 },
  ]);

  assert.strictEqual(warnings.length, 2);
  assert.deepStrictEqual(
    warnings.map((w) => w.source),
    ['route-rear-singbox', 'sing-box'],
    'сортировка не по свежести'
  );
});

test('occurrences are counted and the sample comes from the latest one', () => {
  /** В свежей строке свежие числа; старые уже ничего не говорят о текущем состоянии. */
  const warnings = aggregateWarnings([
    { kind: 'quic_recv_buffer', source: 'sing-box', sample: 'старое', atUnixMs: 1000 },
    { kind: 'quic_recv_buffer', source: 'sing-box', sample: 'свежее', atUnixMs: 5000 },
    { kind: 'quic_recv_buffer', source: 'sing-box', sample: 'среднее', atUnixMs: 3000 },
  ]);

  assert.strictEqual(warnings.length, 1);
  assert.strictEqual(warnings[0].occurrences, 3);
  assert.strictEqual(warnings[0].sample, 'свежее');
  assert.strictEqual(warnings[0].lastSeenUnixMs, 5000);
});

test('both the unit pass and the kernel pass are run', () => {
  const calls: string[][] = [];
  const run = (async (_file: string, args: string[]) => {
    calls.push(args);
    return { stdout: '', stderr: '' };
  }) as never;

  return collectJournalWarnings(run, 24, new Date(2026, 8, 4)).then(() => {
    assert.strictEqual(calls.length, 2, 'проходов не два');
    assert.ok(calls[0].includes('-u'), 'первый проход не по юнитам');
    assert.ok(calls[0].includes('sing-box') && calls[0].includes('route-rear-singbox'), 'тыловой юнит не опрашивается');
    assert.ok(calls[1].includes('-k'), 'второй проход не по ядру');
    assert.ok(!calls[1].includes('-u'), '-k и -u в одном вызове взаимоисключающи');
  });
});

test('exit code 1 with no output means "nothing found", not a failure', async () => {
  /**
   * journalctl отдаёт 1, когда совпадений нет, — и это самый частый исход. Считать его ошибкой
   * значило бы писать в журнал предупреждение на каждом холостом проходе, то есть засорять ровно
   * тот журнал, в котором мы ищем.
   */
  const run = (async () => {
    const err: any = new Error('Command failed');
    err.code = 1;
    err.stdout = '';
    throw err;
  }) as never;

  const warnings = await collectJournalWarnings(run, 24, new Date());

  assert.deepStrictEqual(warnings, []);
});

test('output attached to a non-zero exit is still parsed', async () => {
  /**
   * journalctl умеет отдать и совпадения, и ненулевой код одновременно (например когда часть
   * журнала недоступна). Выбросить готовый результат из-за кода возврата значило бы потерять
   * найденное предупреждение.
   */
  const run = (async () => {
    const err: any = new Error('Command failed');
    err.code = 1;
    err.stdout = journalLine({
      MESSAGE: QUIC_RECV_LINE,
      _SYSTEMD_UNIT: 'sing-box.service',
      __REALTIME_TIMESTAMP: '1788000000000000',
    });
    throw err;
  }) as never;

  const warnings = await collectJournalWarnings(run, 24, new Date());

  assert.strictEqual(warnings.length, 1);
  assert.strictEqual(warnings[0].kind, 'quic_recv_buffer');
  assert.strictEqual(warnings[0].occurrences, 2, 'оба прохода отдали одно и то же — счёт должен это отразить');
});

test('each pass carries a hard time limit and is killed outright', () => {
  /**
   * ПОЧЕМУ ЭТО ВАЖНО. Потолка времени тут не было вовсе, и отмена gRPC-вызова по дедлайну
   * дочерний процесс НЕ убивает: journalctl продолжал скрести журнал сам по себе. На живой ноде
   * (2026-09-05) проход по юнитам занимал 65 секунд при дедлайне вызова в 60 — оркестратор получал
   * DEADLINE_EXCEEDED каждые 15 минут.
   *
   * Проверяем оба поля. `timeout` без `killSignal` оставил бы SIGTERM, а journalctl в разгаре
   * чтения сжатого журнала на мягкий сигнал может не ответить — то есть ровно тот случай, ради
   * которого потолок и заводится, он бы не покрыл.
   */
  const opts: Array<Record<string, unknown>> = [];
  const run = (async (_file: string, _args: string[], options: Record<string, unknown>) => {
    opts.push(options);
    return { stdout: '', stderr: '' };
  }) as never;

  return collectJournalWarnings(run, 24, new Date()).then(() => {
    assert.strictEqual(opts.length, 2, 'проходов не два');
    for (const [i, o] of opts.entries()) {
      assert.ok(typeof o.timeout === 'number' && o.timeout > 0, `у прохода ${i} нет потолка времени`);
      assert.strictEqual(o.killSignal, 'SIGKILL', `проход ${i} снимается мягким сигналом`);
    }
  });
});

test('two passes together fit inside the RPC deadline', () => {
  /**
   * Потолок выбран не на глаз: проходы последовательные, дедлайн вызова — 60 секунд, и сумма
   * обязана оставлять запас на сам gRPC. Тест держит именно это соотношение, а не конкретное
   * число: поднимут потолок до 40 секунд «чтобы успевало» — и вернётся та же ошибка, только
   * объяснить её будет некому.
   */
  const RPC_DEADLINE_MS = 60_000;
  const opts: number[] = [];
  const run = (async (_file: string, _args: string[], options: Record<string, unknown>) => {
    opts.push(options.timeout as number);
    return { stdout: '', stderr: '' };
  }) as never;

  return collectJournalWarnings(run, 24, new Date()).then(() => {
    const total = opts.reduce((a, b) => a + b, 0);
    assert.ok(
      total < RPC_DEADLINE_MS * 0.8,
      `сумма потолков ${total} мс не оставляет запаса под дедлайн ${RPC_DEADLINE_MS} мс`
    );
  });
});

test('a pass killed by the time limit does not sink the other one', async () => {
  /**
   * Проходов два, и дорогой из них ровно один: на живой ноде поход по юнитам занимал 65 секунд, а
   * по ядру — 0,3. Уронить из-за первого второй значило бы терять шесть классов предупреждений из
   * семи там, где они как раз и водятся.
   */
  let call = 0;
  const run = (async () => {
    call += 1;
    if (call === 1) {
      const err: any = new Error('Command failed');
      err.killed = true;
      err.signal = 'SIGKILL';
      err.stdout = '';
      throw err;
    }
    return {
      stdout: JSON.stringify({
        MESSAGE: 'nf_conntrack: table full, dropping packet',
        __REALTIME_TIMESTAMP: '1000000',
        _SYSTEMD_UNIT: 'kernel',
      }),
      stderr: '',
    };
  }) as never;

  const warnings = await collectJournalWarnings(run, 24, new Date());

  assert.strictEqual(call, 2, 'второй проход не состоялся');
  assert.strictEqual(warnings.length, 1, 'ядерное предупреждение потеряно вместе с сорванным проходом');
});
