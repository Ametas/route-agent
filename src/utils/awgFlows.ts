import * as fs from 'fs/promises';
import pino from 'pino';
import { execAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Наблюдение за трафиком AWG — слепая зона всего остального контура.
 *
 * **ПОЧЕМУ ЭТОГО НЕ БЫЛО.** Клиентский AWG уходит с узла ядерным NAT'ом:
 *
 *   iptables -t nat -A POSTROUTING -o <iface> -j MASQUERADE
 *
 * То есть `устройство → awg0 → ядро → интернет`, МИНУЯ sing-box. А всё наше наблюдение построено
 * на `GetSingBoxConnections` — он спрашивает у sing-box его таблицу соединений, куда AWG-трафик не
 * попадает по построению. В снимке для вотчера слова `awg` не встречалось ни разу: абоненты на AWG
 * были целиком вне контура — ни наблюдения, ни ограничения.
 *
 * **ДВА ИСТОЧНИКА, И ОНИ РАЗНЫЕ ПО НАДЁЖНОСТИ.**
 *
 * 1. `awg show <iface> dump` — счётчики приёма и отдачи по КАЖДОМУ пиру. Есть всегда. Мы этот вызов
 *    уже делали ради подсчёта живых пиров, а колонки со счётчиками выбрасывали.
 * 2. `/proc/net/nf_conntrack` — таблица соединений ядра: назначение, порт, протокол. Даёт форму
 *    потока, но байты в ней появляются только при включённом `nf_conntrack_acct`, а он по умолчанию
 *    выключен. Поэтому байты берём из первого источника, а из второго — только форму.
 *
 * **ПОЧЕМУ `/proc`, А НЕ `conntrack -L`.** Утилита живёт в пакете `conntrack-tools`, которого на
 * узлах может не быть, а `install.sh` его не ставит. Файл же читается без единой зависимости, и
 * модуль ядра гарантированно загружен — его требует наш собственный `MASQUERADE`.
 *
 * **АТРИБУЦИЯ ЗДЕСЬ ЛУЧШЕ, ЧЕМ НА ПРОКСИ-СТОРОНЕ.** Там всё приходит с `127.0.0.1`, и абонента
 * приходится вытаскивать из метаданных. Тут у каждого пира свой адрес в туннеле, выданный нами, и
 * публичный ключ рядом в той же строке — соответствие однозначное.
 */

/** Один пир: кто он и сколько прокачал с момента поднятия интерфейса. */
export interface AwgPeer {
  publicKey: string;
  /**
   * Откуда пир пришёл — адрес и порт снаружи туннеля.
   *
   * ЕДИНСТВЕННЫЙ ДОСТУПНЫЙ ПРИЗНАК РАЗДЕЛЁННОГО КЛЮЧА. Конфиг AWG самодостаточен: приватный ключ,
   * адрес, порт — вставил в приложение и работаешь. Запроса подписки при этом НЕ ПРОИСХОДИТ, то
   * есть переслать конфиг другому человеку мы никак не заметим. А WireGuard второму клиенту с тем
   * же ключом не отказывает — он просто переставляет endpoint пира на того, кто прислал более
   * свежее рукопожатие. Снаружи это выглядит нестабильной связью, а не разделённым ключом.
   *
   * Отсюда: endpoint, скачущий между разными адресами (тем более между странами), — это всё, что
   * вообще видно без DPI. `(none)` означает, что пир ещё ни разу не подключался.
   */
  endpoint: string;
  /** Адреса пира внутри туннеля — по ним же соединения из conntrack привязываются к нему. */
  tunnelIps: string[];
  /** Принято ОТ пира, то есть его отдача. */
  rxBytes: number;
  /** Отдано ПИРУ, то есть его приём. */
  txBytes: number;
  latestHandshakeUnix: number;
}

/** Одно соединение из таблицы ядра, уже привязанное к пиру. */
export interface AwgFlow {
  publicKey: string;
  tunnelIp: string;
  protocol: string;
  destinationIp: string;
  destinationPort: number;
}

/**
 * Разбор `awg show <iface> dump`.
 *
 * Формат строки пира: `pubkey psk endpoint allowed-ips handshake rx tx keepalive`. Первая строка —
 * это сам интерфейс, а не пир, и её надо пропускать.
 *
 * Чистая функция, чтобы разбор проверялся без живого интерфейса.
 */
export function parseAwgDump(stdout: string): AwgPeer[] {
  const peers: AwgPeer[] = [];

  const lines = stdout.trim().split('\n');
  // Первая строка описывает интерфейс (приватный ключ, порт, fwmark) — пиров там нет.
  for (const raw of lines.slice(1)) {
    const line = raw.trim();
    if (!line) continue;

    const parts = line.split(/\s+/);
    // Восемь колонок — полная строка пира. Короче — обрезанный вывод, и гадать по нему нельзя.
    if (parts.length < 8) continue;

    const rxBytes = Number.parseInt(parts[5]!, 10);
    const txBytes = Number.parseInt(parts[6]!, 10);
    const latestHandshakeUnix = Number.parseInt(parts[4]!, 10);

    peers.push({
      publicKey: parts[0]!,
      endpoint: parts[2] === '(none)' ? '' : parts[2]!,
      // `(none)` встречается у пира без разрешённых адресов — такому соединения не привязать.
      tunnelIps: parts[3] === '(none)' ? [] : parts[3]!.split(',').map((ip) => ip.split('/')[0]!),
      rxBytes: Number.isFinite(rxBytes) ? rxBytes : 0,
      txBytes: Number.isFinite(txBytes) ? txBytes : 0,
      latestHandshakeUnix: Number.isFinite(latestHandshakeUnix) ? latestHandshakeUnix : 0,
    });
  }

  return peers;
}

/**
 * Разбор `/proc/net/nf_conntrack`, оставляющий только соединения НАШИХ пиров.
 *
 * Строка выглядит так (поля после протокола зависят от его состояния):
 *   ipv4 2 tcp 6 431999 ESTABLISHED src=100.64.0.2 dst=1.2.3.4 sport=51234 dport=443 ...
 *
 * ПОЧЕМУ ФИЛЬТР ПО АДРЕСАМ ПИРОВ, А НЕ ПО ПОДСЕТИ. Тот же проход и отбирает, и приписывает
 * соединение владельцу: подсеть сказала бы «это чей-то из AWG», а адрес пира говорит «этого».
 * Заодно из выборки выпадает всё остальное, что живёт в таблице ядра, — а там весь трафик узла.
 *
 * ПЕРВАЯ пара `src=/dst=` — исходное направление; вторая описывает обратное после NAT, и брать её
 * нельзя: назначение там подменено на адрес самого узла.
 */
/** Транспортные протоколы, которые вообще попадают в таблицу соединений. */
const L4_PROTOCOLS = new Set(['tcp', 'udp', 'icmp', 'icmpv6', 'udplite', 'dccp', 'sctp', 'gre']);

/**
 * Протокол строки — поиском по первым колонкам, а не по фиксированному индексу.
 *
 * ДВА ФОРМАТА, И ОНИ СДВИНУТЫ ДРУГ ОТНОСИТЕЛЬНО ДРУГА. `/proc/net/nf_conntrack` начинает строку с
 * семейства адресов (`ipv4 2 tcp 6 …`), вывод `conntrack -L` — сразу с протокола (`tcp 6 …`).
 * Прежний разбор брал третью колонку и на выводе утилиты возвращал бы номер протокола вместо имени.
 */
function protocolOf(parts: string[]): string {
  for (const token of parts.slice(0, 4)) {
    if (L4_PROTOCOLS.has(token)) return token;
  }
  return 'unknown';
}

export function parseConntrack(contents: string, tunnelIpToPeer: Map<string, string>): AwgFlow[] {
  const flows: AwgFlow[] = [];

  for (const line of contents.split('\n')) {
    if (!line) continue;

    const src = /\bsrc=(\S+)/.exec(line);
    if (!src) continue;
    const publicKey = tunnelIpToPeer.get(src[1]!);
    if (publicKey === undefined) continue;

    const dst = /\bdst=(\S+)/.exec(line);
    const dport = /\bdport=(\d+)/.exec(line);
    if (!dst || !dport) continue;

    const protocol = protocolOf(line.trim().split(/\s+/));

    flows.push({
      publicKey,
      tunnelIp: src[1]!,
      protocol,
      destinationIp: dst[1]!,
      destinationPort: Number.parseInt(dport[1]!, 10),
    });
  }

  return flows;
}

export interface AwgObservation {
  peers: AwgPeer[];
  flows: AwgFlow[];
  /**
   * Прочиталась ли таблица ядра. Отличать «соединений нет» от «посмотреть не удалось» обязательно:
   * иначе узел без доступа к `/proc/net/nf_conntrack` выглядел бы идеально тихим.
   */
  conntrackAvailable: boolean;
}

const CONNTRACK_PATH = '/proc/net/nf_conntrack';

/** Снимок состояния AWG: пиры со счётчиками и их соединения из таблицы ядра. */
export async function collectAwgObservation(iface = 'awg0'): Promise<AwgObservation> {
  let peers: AwgPeer[] = [];
  try {
    const { stdout } = await execAsync(`awg show ${iface} dump`);
    peers = parseAwgDump(stdout);
  } catch (err: unknown) {
    // Интерфейса нет — на узле без AWG это штатное состояние, а не поломка.
    logger.debug(
      { err: err instanceof Error ? err.message : String(err), iface },
      'AWG interface not available, no observation to report'
    );
    return { peers: [], flows: [], conntrackAvailable: false };
  }

  const tunnelIpToPeer = new Map<string, string>();
  for (const peer of peers) {
    for (const ip of peer.tunnelIps) tunnelIpToPeer.set(ip, peer.publicKey);
  }

  if (tunnelIpToPeer.size === 0) return { peers, flows: [], conntrackAvailable: false };

  const contents = await readConntrackTable();
  if (contents === null) return { peers, flows: [], conntrackAvailable: false };

  return { peers, flows: parseConntrack(contents, tunnelIpToPeer), conntrackAvailable: true };
}

/**
 * Таблица соединений ядра: сначала `/proc`, потом утилита.
 *
 * ПОЧЕМУ ПОНАДОБИЛСЯ ВТОРОЙ ПУТЬ (2026-09-20). `/proc/net/nf_conntrack` даёт опция сборки ядра
 * `CONFIG_NF_CONNTRACK_PROCFS`, и в современных сборках её выключают — proc-интерфейс давно
 * объявлен устаревшим. На нашем узле так и оказалось: модули `nf_conntrack` загружены, таблица
 * ведётся, а файла нет и не будет. Наблюдение при этом молча отдавало ноль соединений: отпечаток
 * базы вотчера показал тринадцать тысяч замеров формы трафика AWG, у всех `flows: 0`, — то есть
 * сигнал, ради которого всё это собиралось, не мог сработать ни разу.
 *
 * ПОРЯДОК ИМЕННО ТАКОЙ. Там, где файл есть, он дешевле: чтение в процессе против запуска утилиты
 * каждые три минуты. Утилита — запасной путь, а не замена.
 *
 * `null` означает «посмотреть не удалось» и доезжает до вотчера флагом `conntrackAvailable: false`.
 * Отличать это от «соединений нет» обязательно: иначе слепой узел выглядел бы идеально тихим.
 */
async function readConntrackTable(): Promise<string | null> {
  try {
    return await fs.readFile(CONNTRACK_PATH, 'utf-8');
  } catch (err: unknown) {
    logger.debug(
      { err: err instanceof Error ? err.message : String(err) },
      'Kernel conntrack procfs unavailable, falling back to the conntrack utility'
    );
  }

  try {
    // Сводку («N flow entries have been shown») утилита пишет в stderr, записи — в stdout.
    const { stdout } = await execAsync('conntrack -L');
    return stdout;
  } catch (err: unknown) {
    logger.debug(
      { err: err instanceof Error ? err.message : String(err) },
      'Kernel conntrack table unreadable — reporting peer counters only'
    );
    return null;
  }
}
