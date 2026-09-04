import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import { config } from '../config.js';
import { execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Профиль ядра под QUIC-нагрузку: буферы сокетов и контроль перегрузки.
 *
 * ЧТО ИЗМЕРЕНО НА ЖИВОЙ НАГРУЖЕННОЙ НОДЕ (2026-09-04, около миллиарда входящих дейтаграмм с
 * загрузки). `RcvbufErrors` = 6 010 649 — это 0,58% входящего UDP, выброшенного из-за переполнения
 * буфера приёма, и ровно столько же составляет `InErrors`: других видов потерь UDP на ноде нет
 * вообще. Для QUIC полпроцента потерь не мелочь — каждая вызывает реакцию контроля перегрузки.
 *
 * ГРАНИЦА ПРОХОДИТ ПО РАЗМЕРУ БУФЕРА, и это видно поимённо через `ss -uanpm` (последнее поле
 * `skmem` — счётчик потерь сокета): все сокеты с `rb212992` теряют — публичный слушатель
 * sing-box на :443, исходящие сокеты xray Ремны (154 и 50 тысяч потерь), loopback-соединения к
 * бэкендам; все сокеты с `rb8388608` не теряют ни одного. Совпадение слишком чистое, чтобы быть
 * совпадением.
 *
 * РЫЧАГ — `rmem_default`, А НЕ `rmem_max`, и это не рассуждение, а замер. Сокет получает
 * `rmem_default`, если никто не звал `setsockopt`, и упирается в `rmem_max`, только если звал.
 * На третьей ноде (только хистерия Ремны, без нашего sing-box) `rmem_max` УЖЕ поднят до 4194304 —
 * а сокеты, которые теряют, всё равно сидят на `rb212992`, то есть ровно на `rmem_default`, и
 * теряют по 500-780 тысяч каждый. Потолок вчетверо выше умолчания не помогает им никак: они до
 * него не тянутся. Поднимать `rmem_max` ради них значило бы поднимать то, чего никто не касается.
 *
 * КТО ИМЕННО ТЕРЯЕТ: исходящие UDP-сокеты, которые xray создаёт под пользовательский трафик наружу.
 * Свой слушатель на :443 он настраивает сам (`rb8388608`, потерь ноль) — и может, потому что
 * `CapEff` его процесса = `a80435fb`, то есть дефолтный набор Docker плюс бит 12,
 * `CAP_NET_ADMIN`. С этой возможностью `SO_RCVBUFFORCE` игнорирует `rmem_max` вовсе (на другой
 * ноде так нашлись сокеты по 14 и 16 МБ при потолке 212992). А вот исходящим сокетам он буфер не
 * ставит вовсе — им достаётся умолчание.
 *
 * `rmem_max` = 7500000 всё же оставлен: это рекомендация вики самого quic-go, стоит он ноль и
 * пригодится тому, кто буфер запрашивает явно.
 *
 * ПОЧЕМУ 1 МБ, А НЕ 7,5. Умолчание достаётся КАЖДОМУ сокету, поэтому первый шаг должен быть
 * скромным и измеримым: впятеро против нынешнего, дальше смотрим на счётчик. Опасение «раздадим
 * память каждому» при этом оказалось преувеличенным — в дампе `ss` у ВСЕХ сокетов поле `r0`,
 * включая восьмимегабайтный: `rb` это лимит, а не выделение, память списывается по мере прихода
 * данных. Радиус поражения к тому же ограничен UDP: у TCP своё автотюнингуемое `tcp_rmem`,
 * которое `rmem_default` перекрывает.
 *
 * СЛЕДСТВИЕ ДЛЯ СБОРА ПРЕДУПРЕЖДЕНИЙ: молчание quic-go про буфер НЕ доказывает, что буфер
 * достаточен, — оно может означать, что процесс взял своё в обход потолка.
 *
 * ЛОВУШКА, НА КОТОРОЙ ЛЕГКО ОШИБИТЬСЯ ВДВОЕ: ядро хранит удвоенное значение и `getsockopt` вернёт
 * 15000000 при запрошенных 7,5 МБ. Но потолок сравнивается с ЗАПРОШЕННЫМ числом, а не с удвоенным,
 * поэтому здесь стоит 7500000 без всякого умножения. Проверено на ядре 6.6: при `rmem_max` = 212992
 * сокет, попросивший 7500000, получает 212992; после поднятия потолка — все 7500000.
 *
 * ЧЕГО ЗДЕСЬ НЕТ И ПОЧЕМУ — каждое исключение стоило измерения, а не рассуждения:
 *
 *   * `netdev_max_backlog` (умолчание 1000) — колонка потерь в `/proc/net/softnet_stat` на той же
 *     нагруженной ноде сплошь нули. Очередь приёма ни разу не переполнялась, поднимать не от чего.
 *   * `ip_local_port_range` — расширение вниз затянуло бы в эфемерный диапазон пять фиксированных
 *     портов ноды (20000 диспетчер, 28080/28081 Clash API фронта и тыла, 29000/29001 инбаунды
 *     тыла). При умолчании 32768-60999 все пять лежат НИЖЕ него и столкнуться не могут; правка
 *     создала бы риск, которого нет, ради выигрыша, которого никто не измерял.
 *   * `udp_mem`, `tcp_rmem`, `tcp_wmem` считаются ядром от объёма памяти при загрузке. Зашитое
 *     число было бы вдвое мимо: на ноде с вдвое меньшей памятью `udp_mem` ровно вдвое меньше.
 *
 * ПРО `default_qdisc`: sysctl действует на ВНОВЬ создаваемые qdisc'и, то есть на существующем
 * интерфейсе очередь сменится только после перезагрузки. Это осознанно — переставлять корневой
 * qdisc живому интерфейсу из агента значит трогать проходящий трафик ради улучшения, которое
 * подождёт. BBR при этом работает сразу: контроль перегрузки выбирается при создании соединения.
 * С ingress-шейпером (`utils/trafficShaper.ts`) конфликта нет — тот живёт на другой стороне.
 */
export interface KernelTuningEntry {
  key: string;
  value: string;
  /** Зачем эта строка нужна — идёт комментарием в файл, чтобы на ноде не гадали. */
  why: string;
}

export const KERNEL_TUNING: readonly KernelTuningEntry[] = [
  {
    key: 'net.core.rmem_default',
    value: '1048576',
    why: 'ГЛАВНАЯ строка: буфер сокетов, которые его не запрашивают. Именно они теряют пакеты (по 500-780 тысяч на сокет), сидя на умолчании 212992 при уже поднятом потолке',
  },
  {
    key: 'net.core.wmem_default',
    value: '1048576',
    why: 'то же для отправки: SndbufErrors на нагруженной ноде 316 тысяч',
  },
  {
    key: 'net.core.rmem_max',
    value: '7500000',
    why: 'потолок для тех, кто буфер запрашивает явно. Значение из вики quic-go; сам по себе теряющие сокеты не спасает — они до потолка не тянутся',
  },
  {
    key: 'net.core.wmem_max',
    value: '7500000',
    why: 'то же для отправки',
  },
  {
    key: 'net.ipv4.tcp_congestion_control',
    value: 'bbr',
    why: 'на нетюненой ноде cubic; bbr доступен в ядре (tcp_available_congestion_control: reno cubic bbr)',
  },
  {
    key: 'net.core.default_qdisc',
    value: 'fq',
    why: 'пара к bbr: пейсинг делает очередь, а не ядро. На нетюненой ноде fq_codel. Действует на вновь создаваемые qdisc, то есть с перезагрузки',
  },
];

export interface KernelTuningKeyResult {
  key: string;
  applied: boolean;
  /** Значение, которое ядро отдало ПОСЛЕ применения. `null` — прочитать не удалось. */
  effective: string | null;
  error?: string;
}

export interface KernelTuningResult {
  /** Переписан ли файл в /etc/sysctl.d. `false` — уже лежал ровно такой. */
  fileChanged: boolean;
  keys: KernelTuningKeyResult[];
}

/**
 * Файл для `/etc/sysctl.d`. Нужен не вместо применения, а вместе с ним: `sysctl -w` живёт до
 * перезагрузки, файл переживает её даже если агент в этот момент не поднялся.
 */
export function renderSysctlFile(entries: readonly KernelTuningEntry[] = KERNEL_TUNING): string {
  const body = entries.map((e) => `# ${e.why}\n${e.key} = ${e.value}`).join('\n\n');
  return `# Управляется route-agent, правки будут перезаписаны при следующем старте агента.\n\n${body}\n`;
}

/**
 * Применяет профиль и возвращает, что из этого вышло.
 *
 * ПО КЛЮЧАМ, А НЕ `sysctl -p` ЦЕЛИКОМ: на OpenVZ и в контейнерах часть ключей доступна только для
 * чтения, и один такой не должен уносить с собой остальные. По той же причине неудача — это
 * предупреждение, а не отказ: агент, который не стартует из-за недоступного sysctl, хуже агента с
 * неоптимальными буферами.
 *
 * Значение читается ОБРАТНО после записи, а не берётся из намерения: ядро может принять команду и
 * округлить величину, и знать надо то, что там оказалось на самом деле.
 */
export async function applyKernelTuning(
  runExecFile: typeof execFileAsync = execFileAsync,
  entries: readonly KernelTuningEntry[] = KERNEL_TUNING
): Promise<KernelTuningResult> {
  const fileChanged = await writeSysctlFile(entries);
  const keys: KernelTuningKeyResult[] = [];

  for (const entry of entries) {
    try {
      await runExecFile('sysctl', ['-w', `${entry.key}=${entry.value}`]);
    } catch (err: any) {
      const message = err?.message ? String(err.message) : 'unknown error';
      logger.warn({ key: entry.key, err: message }, 'Kernel tuning: sysctl key could not be set');
      keys.push({ key: entry.key, applied: false, effective: null, error: message });
      continue;
    }

    let effective: string | null = null;
    try {
      const { stdout } = await runExecFile('sysctl', ['-n', entry.key]);
      effective = String(stdout).trim().replace(/\s+/g, ' ');
    } catch {
      // Записалось, а прочитать не смогли — не повод считать применение неудачным.
    }
    keys.push({ key: entry.key, applied: true, effective });
  }

  const failed = keys.filter((k) => !k.applied).map((k) => k.key);
  logger.info(
    { fileChanged, applied: keys.length - failed.length, failed },
    'Kernel tuning: network buffer profile applied'
  );

  return { fileChanged, keys };
}

/** Пишем только при отличии — тем же приёмом, что и systemd-юниты рядом. */
async function writeSysctlFile(entries: readonly KernelTuningEntry[]): Promise<boolean> {
  const target = config.SYSCTL_PROFILE_PATH;
  const expected = renderSysctlFile(entries);

  const existing = await fs.readFile(target, 'utf-8').catch(() => null);
  if (existing === expected) return false;

  await fs.mkdir(path.dirname(target), { recursive: true });
  await fs.writeFile(target, expected, 'utf-8');
  logger.info({ path: target }, 'Kernel tuning: sysctl profile written');
  return true;
}
