import path from 'path';
import pino from 'pino';
import { config } from '../config.js';
import { execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Сбор предупреждений о ТИХОЙ деградации из журналов самой ноды.
 *
 * КЛАСС ПРОБЛЕМЫ, РАДИ КОТОРОГО ЭТО НАПИСАНО. Всё работает, ничего не падает, метрики зелёные — а
 * работает хуже, чем должно, и узнать об этом можно только зайдя на ноду и грепнув журнал. Живой
 * пример: quic-go внутри sing-box просит буфер приёма на 7,5 МБ, ядро молча срезает его до 208 КБ и
 * пишет ОДНУ строку при подъёме сокета. Ни телеметрия, ни здоровье ноды этого не видят.
 *
 * ПОЧЕМУ ФИЛЬТР ОТДАН journalctl. У него есть `--grep`, и он делает отбор на ноде. Альтернатива —
 * вычитывать журнал целиком и фильтровать у себя — означала бы гонять мегабайты по каналу, за
 * который владелец платит, ради нескольких строк.
 *
 * БЕЗ СОСТОЯНИЯ, СОЗНАТЕЛЬНО. Возвращается всё, что видно в окне оглядки, с числом повторов — агент
 * НЕ помнит, о чём уже докладывал. Курсор журнала пришлось бы хранить на диске, согласовывать с
 * неизвестным числом потребителей, и он молча проглатывал бы предупреждение при любом рассинхроне.
 * Дедупликация — забота оркестратора, потому что решение «беспокоить ли человека» принимает он.
 */

export interface WarningPattern {
  /** Стабильный идентификатор класса. Оркестратор дедуплицирует по нему, поэтому он не меняется. */
  kind: string;
  /** Регулярное выражение для `journalctl --grep` И для разбора на нашей стороне. */
  regex: string;
}

/**
 * Реестр — точка расширения. Добавить класс значит дописать сюда строку: и отбор в journalctl, и
 * классификация собираются из этого же списка, второго места нет.
 *
 * Порядок значим: побеждает ПЕРВОЕ совпадение, поэтому частные образцы стоят раньше общих.
 */
export const WARNING_PATTERNS: readonly WarningPattern[] = [
  // quic-go, дословно из его исходников: строка содержит и желаемый, и полученный размер, и это
  // готовый диагноз.
  //
  // НО МОЛЧАНИЕ ЗДЕСЬ НИЧЕГО НЕ ДОКАЗЫВАЕТ, и это выяснилось на живых нодах (2026-09-04). Процесс
  // с `CAP_NET_ADMIN` — а sing-box работает от root — берёт буфер через `SO_RCVBUFFORCE` в обход
  // `net.core.rmem_max` и предупреждать ему не о чем: на ноде с потолком 212992 нашлись сокеты по
  // 14 и 16 МБ. Класс остаётся верным для непривилегированного случая (процесс в контейнере без
  // этой возможности), но считать пустой журнал признаком здоровья буферов нельзя — размер надо
  // смотреть у самих сокетов (`ss -uanpm`, поля `rb` и `d`).
  { kind: 'quic_recv_buffer', regex: 'failed to sufficiently increase receive buffer size' },
  { kind: 'quic_send_buffer', regex: 'failed to sufficiently increase send buffer size' },
  // Второй, более грубый отказ того же места: буфер не увеличился вовсе.
  { kind: 'quic_buffer_not_increased', regex: 'failed to increase (?:receive|send) buffer size' },
  // Ядро выбрасывает пакеты, потому что таблица соединений заполнена. Снаружи выглядит как
  // случайные обрывы у части абонентов.
  { kind: 'conntrack_table_full', regex: 'nf_conntrack: table full' },
  // Упёрлись в лимит файловых дескрипторов: новые соединения не принимаются, старые живут.
  { kind: 'too_many_open_files', regex: 'too many open files' },
  // Ядру не хватает памяти под сокеты — та же картина, что у conntrack, но по другой причине.
  { kind: 'out_of_socket_memory', regex: 'Out of socket memory' },
  // Переполнение ARP-таблицы. На egress-ноде с большим числом соседей это реальный отказ связи.
  { kind: 'neighbour_table_overflow', regex: 'neighbour table overflow' },
];

export interface JournalWarning {
  kind: string;
  source: string;
  sample: string;
  occurrences: number;
  lastSeenUnixMs: number;
}

/** Длина сохраняемой строки. Числа в предупреждениях стоят в начале, хвост можно терять. */
const SAMPLE_LIMIT = 300;

/** Потолок разбираемых строк на один вызов: защита от журнала, забитого одним и тем же. */
const LINE_LIMIT = 2000;

/**
 * Потолок времени на ОДИН проход journalctl.
 *
 * Раньше его не было вовсе, и это был не недосмотр в мелочи, а способ повесить вызов навсегда:
 * отмена gRPC по дедлайну дочерний процесс не убивает, он продолжает скрести журнал сам по себе.
 * На живой ноде (2026-09-05) проход по юнитам занимал 65 секунд при дедлайне вызова в 60 — то есть
 * оркестратор получал `DEADLINE_EXCEEDED` каждые 15 минут, а на ноде оставался работающий скан.
 *
 * Двадцать секунд взяты от дедлайна вызова, а не с потолка: проходов два, они последовательные,
 * значит худший случай — сорок секунд, и остаётся запас на сам gRPC. Не уложились — докладываем
 * без этого прохода: половина сведений лучше, чем сорванный вызов и висящий процесс.
 *
 * `SIGKILL`, а не `SIGTERM`: journalctl в разгаре чтения сжатого журнала на мягкий сигнал может не
 * ответить, а смысл потолка в том, чтобы процесс ГАРАНТИРОВАННО не пережил свой вызов.
 */
const PASS_TIMEOUT_MS = 20_000;

/**
 * Объединённый образец для `--grep`.
 *
 * Один вызов journalctl вместо вызова на класс: отбор всё равно идёт по одному проходу журнала, а
 * различить классы можно и потом, по той же таблице.
 */
export function buildGrepPattern(patterns: readonly WarningPattern[] = WARNING_PATTERNS): string {
  return patterns.map((p) => `(?:${p.regex})`).join('|');
}

/** К какому классу относится строка. `null` — ни к какому (журнал отдал лишнее). */
export function classifyLine(
  message: string,
  patterns: readonly WarningPattern[] = WARNING_PATTERNS
): string | null {
  for (const pattern of patterns) {
    if (new RegExp(pattern.regex, 'i').test(message)) return pattern.kind;
  }
  return null;
}

/**
 * Метка `--since` для journalctl.
 *
 * Абсолютная, а не относительная (`-24h`): относительные формы systemd понимает по-разному от
 * версии к версии, а `YYYY-MM-DD HH:MM:SS` читается однозначно и, главное, проверяема тестом.
 */
export function sinceArgument(lookbackHours: number, now: Date = new Date()): string {
  const from = new Date(now.getTime() - lookbackHours * 3600_000);
  const pad = (n: number): string => String(n).padStart(2, '0');
  return (
    `${from.getFullYear()}-${pad(from.getMonth() + 1)}-${pad(from.getDate())} ` +
    `${pad(from.getHours())}:${pad(from.getMinutes())}:${pad(from.getSeconds())}`
  );
}

interface JournalEntry {
  MESSAGE?: unknown;
  __REALTIME_TIMESTAMP?: unknown;
  _SYSTEMD_UNIT?: unknown;
}

/**
 * Разбор вывода `--output=json`: строка на запись.
 *
 * `MESSAGE` бывает массивом байтов, когда в строке невалидный UTF-8 — такие пропускаем, а не
 * пытаемся склеить: диагноз в них всё равно не прочитать, а гадать о кодировке здесь незачем.
 */
export function parseJournalJson(stdout: string, fallbackSource: string): Array<{
  kind: string;
  source: string;
  sample: string;
  atUnixMs: number;
}> {
  const out: Array<{ kind: string; source: string; sample: string; atUnixMs: number }> = [];

  for (const line of stdout.split('\n').slice(0, LINE_LIMIT)) {
    const trimmed = line.trim();
    if (!trimmed) continue;

    let entry: JournalEntry;
    try {
      entry = JSON.parse(trimmed) as JournalEntry;
    } catch {
      continue;
    }

    if (typeof entry.MESSAGE !== 'string') continue;
    const kind = classifyLine(entry.MESSAGE);
    if (!kind) continue;

    const unit = typeof entry._SYSTEMD_UNIT === 'string' ? entry._SYSTEMD_UNIT : '';
    const source = unit ? unit.replace(/\.service$/, '') : fallbackSource;

    // __REALTIME_TIMESTAMP — микросекунды, строкой. Отсутствие метки не повод терять запись.
    const micros = Number(entry.__REALTIME_TIMESTAMP);
    const atUnixMs = Number.isFinite(micros) && micros > 0 ? Math.floor(micros / 1000) : Date.now();

    out.push({ kind, source, sample: entry.MESSAGE.slice(0, SAMPLE_LIMIT), atUnixMs });
  }

  return out;
}

/**
 * Сворачивает строки в классы.
 *
 * Ключ — пара «класс + источник»: одно и то же предупреждение у фронтового и у тылового sing-box
 * это два разных факта, и слить их значило бы потерять половину.
 *
 * Образец берётся от ПОСЛЕДНЕГО вхождения: в нём свежие числа, а старые уже неинтересны.
 */
export function aggregateWarnings(
  entries: ReadonlyArray<{ kind: string; source: string; sample: string; atUnixMs: number }>
): JournalWarning[] {
  const byKey = new Map<string, JournalWarning>();

  for (const entry of entries) {
    const key = `${entry.kind}|${entry.source}`;
    const existing = byKey.get(key);

    if (!existing) {
      byKey.set(key, {
        kind: entry.kind,
        source: entry.source,
        sample: entry.sample,
        occurrences: 1,
        lastSeenUnixMs: entry.atUnixMs,
      });
      continue;
    }

    existing.occurrences += 1;
    if (entry.atUnixMs >= existing.lastSeenUnixMs) {
      existing.lastSeenUnixMs = entry.atUnixMs;
      existing.sample = entry.sample;
    }
  }

  return [...byKey.values()].sort((a, b) => b.lastSeenUnixMs - a.lastSeenUnixMs);
}

/** Юниты, чьи журналы имеет смысл смотреть: те, которыми управляем мы. */
export function managedUnits(): string[] {
  return [
    path.basename(config.SINGBOX_UNIT_FILE_PATH, '.service'),
    path.basename(config.REAR_SINGBOX_UNIT_FILE_PATH, '.service'),
    'caddy',
  ];
}

/**
 * Два прохода: по юнитам и по ядру.
 *
 * Раздельно потому, что `-k` и `-u` в journalctl взаимоисключающи — ядерные сообщения не
 * принадлежат ни одному юниту. Отказ любого прохода не отменяет другой: journalctl может
 * отсутствовать или быть урезан в контейнере, и остаться совсем без доклада хуже, чем с половиной.
 */
export async function collectJournalWarnings(
  runExecFile: typeof execFileAsync = execFileAsync,
  lookbackHours: number = config.NODE_WARNING_LOOKBACK_HOURS,
  now: Date = new Date()
): Promise<JournalWarning[]> {
  const since = sinceArgument(lookbackHours, now);
  const grep = buildGrepPattern();
  const common = [
    '--since',
    since,
    '--grep',
    grep,
    '--case-sensitive=false',
    '--output=json',
    '--no-pager',
    `--lines=${LINE_LIMIT}`,
  ];

  const unitArgs = managedUnits().flatMap((unit) => ['-u', unit]);

  const passes: Array<{ args: string[]; fallbackSource: string }> = [
    { args: [...unitArgs, ...common], fallbackSource: 'unknown' },
    { args: ['-k', ...common], fallbackSource: 'kernel' },
  ];

  const collected: Array<{ kind: string; source: string; sample: string; atUnixMs: number }> = [];

  for (const pass of passes) {
    try {
      const { stdout } = await runExecFile('journalctl', pass.args, {
        maxBuffer: 8 * 1024 * 1024,
        timeout: PASS_TIMEOUT_MS,
        killSignal: 'SIGKILL',
      });
      collected.push(...parseJournalJson(String(stdout), pass.fallbackSource));
    } catch (err: any) {
      // journalctl отдаёт код 1, когда совпадений НЕ НАЙДЕНО — это не ошибка, а норма и самый
      // частый исход. Отличаем по пустому stdout: писать в лог на каждом холостом проходе значило
      // бы засорить журнал ровно тем, что мы в нём же ищем.
      const stdout = err?.stdout ? String(err.stdout) : '';
      if (stdout.trim()) {
        collected.push(...parseJournalJson(stdout, pass.fallbackSource));
        continue;
      }
      /**
       * Срабатывание потолка — отдельная новость, а не «проход не удался».
       *
       * Смысл различения не в формулировке: «не уложились» означает, что журнал на этой ноде
       * слишком велик для окна оглядки, и лечится это объёмом журнала или окном. «Не удалось» —
       * что journalctl отсутствует или урезан. Свалить их в одну строку значило бы разбирать
       * причину заново при каждом случае.
       */
      if (err?.killed) {
        logger.warn(
          { timeoutMs: PASS_TIMEOUT_MS, source: pass.fallbackSource },
          'Journal warning scan pass hit the time limit — reporting without it'
        );
        continue;
      }
      if (err?.code !== 1) {
        logger.warn({ err: err?.message, args: pass.args.join(' ') }, 'Journal warning scan pass failed');
      }
    }
  }

  return aggregateWarnings(collected);
}
