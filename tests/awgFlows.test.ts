// tests/awgFlows.test.ts
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { parseAwgDump, parseConntrack } from '../src/utils/awgFlows.js';

/**
 * Наблюдение за трафиком AWG.
 *
 * ЗАЧЕМ ЭТО ПОЯВИЛОСЬ. Клиентский AWG уходит с узла ядерным NAT'ом, МИНУЯ sing-box, а всё наше
 * наблюдение построено на его таблице соединений. В снимке для вотчера слова `awg` не встречалось
 * ни разу: абоненты на AWG были вне контура целиком — ни наблюдения, ни ограничения.
 *
 * Разбор вынесен чистыми функциями: живой интерфейс и таблицу ядра в тестах не поднять.
 */

/** Вывод `awg show awg0 dump`: первая строка — интерфейс, дальше пиры. */
const DUMP = [
  'privkey\tpubkey\t51820\toff',
  'peerA\t(none)\t203.0.113.10:51820\t100.64.0.2/32\t1788880000\t1500000\t900000\t25',
  'peerB\t(none)\t203.0.113.11:51820\t100.64.0.3/32,100.64.0.4/32\t1788880100\t80\t120\toff',
].join('\n');

describe('разбор состояния AWG', () => {
  it('пропускает строку интерфейса и читает пиров', () => {
    const peers = parseAwgDump(DUMP);

    assert.equal(peers.length, 2, 'строка интерфейса принята за пира или пир потерян');
    assert.equal(peers[0]!.publicKey, 'peerA');
  });

  /**
   * ГЛАВНОЕ, РАДИ ЧЕГО РАЗБОР ПЕРЕПИСАН. Эти две колонки мы читали и ВЫБРАСЫВАЛИ: старый парсер брал
   * из `dump` только время рукопожатия. А в них подпись торрента — устойчивая симметрия отдачи и
   * приёма, тогда как просмотр и закачка асимметричны.
   */
  it('читает счётчики приёма и отдачи', () => {
    const [peerA] = parseAwgDump(DUMP);

    assert.equal(peerA!.rxBytes, 1500000, 'отдача пира потеряна');
    assert.equal(peerA!.txBytes, 900000, 'приём пира потерян');
  });

  /**
   * ENDPOINT — единственный доступный признак разделённого ключа. Конфиг AWG самодостаточен и
   * подписку не запрашивает, поэтому пересылку его другому человеку иначе не заметить вовсе:
   * WireGuard второму клиенту с тем же ключом не отказывает, он лишь переставляет endpoint на
   * того, кто прислал более свежее рукопожатие.
   */
  it('читает endpoint, а у неподключавшегося пира оставляет его пустым', () => {
    const peers = parseAwgDump(DUMP);

    assert.equal(peers[0]!.endpoint, '203.0.113.10:51820');
    assert.equal(parseAwgDump('iface\nnever\t(none)\t(none)\t100.64.0.9/32\t0\t0\t0\toff')[0]!.endpoint, '');
  });

  it('разбирает несколько туннельных адресов у одного пира', () => {
    const peerB = parseAwgDump(DUMP)[1]!;

    assert.deepEqual(peerB.tunnelIps, ['100.64.0.3', '100.64.0.4'], 'маска не срезана или адреса не разделены');
  });

  /** `(none)` — пир без разрешённых адресов; соединения ему не привязать, но и падать не на чем. */
  it('переживает пира без адресов', () => {
    const peers = parseAwgDump('iface\nnokeys\t(none)\t-\t(none)\t0\t0\t0\toff');

    assert.deepEqual(peers[0]!.tunnelIps, []);
  });

  it('обрезанную строку не додумывает', () => {
    assert.deepEqual(parseAwgDump('iface\nbroken\t(none)\t203.0.113.1:1'), []);
  });

  it('пустой вывод не роняет разбор', () => {
    for (const junk of ['', '\n', 'только-интерфейс']) {
      assert.deepEqual(parseAwgDump(junk), [], `упало на ${JSON.stringify(junk)}`);
    }
  });
});

/**
 * Строки `/proc/net/nf_conntrack`. Вторая пара `src=/dst=` описывает ОБРАТНОЕ направление после
 * NAT: назначение там подменено на адрес самого узла, и брать её нельзя.
 */
const CONNTRACK = [
  'ipv4     2 tcp      6 431999 ESTABLISHED src=100.64.0.2 dst=149.154.167.41 sport=51234 dport=443 src=149.154.167.41 dst=203.0.113.10 sport=443 dport=51234 [ASSURED] mark=0 use=1',
  'ipv4     2 udp      17 29 src=100.64.0.2 dst=198.51.100.7 sport=6881 dport=6881 src=198.51.100.7 dst=203.0.113.10 sport=6881 dport=6881 mark=0 use=1',
  'ipv4     2 tcp      6 300 ESTABLISHED src=10.9.9.9 dst=1.1.1.1 sport=40000 dport=443 src=1.1.1.1 dst=203.0.113.10 sport=443 dport=40000 [ASSURED] mark=0 use=1',
].join('\n');

const OWNERS = new Map([['100.64.0.2', 'peerA']]);

describe('разбор таблицы соединений ядра', () => {
  it('оставляет только соединения наших пиров', () => {
    const flows = parseConntrack(CONNTRACK, OWNERS);

    assert.equal(flows.length, 2, 'чужое соединение попало в выборку или наше потеряно');
    assert.ok(flows.every((f) => f.publicKey === 'peerA'));
  });

  /**
   * Тот же проход и отбирает, и приписывает владельца: подсеть сказала бы «это чей-то из AWG»,
   * адрес пира говорит «этого».
   */
  it('приписывает соединение владельцу туннельного адреса', () => {
    const [first] = parseConntrack(CONNTRACK, OWNERS);

    assert.equal(first!.publicKey, 'peerA');
    assert.equal(first!.tunnelIp, '100.64.0.2');
  });

  /**
   * ИМЕННО ПЕРВАЯ пара `dst=`. Вторая — обратное направление после NAT, и там стоит адрес узла;
   * возьми мы её, все соединения выглядели бы идущими на сам сервер.
   */
  it('берёт исходное назначение, а не подменённое NAT-ом', () => {
    const [first] = parseConntrack(CONNTRACK, OWNERS);

    assert.equal(first!.destinationIp, '149.154.167.41');
    assert.equal(first!.destinationPort, 443);
    assert.notEqual(first!.destinationIp, '203.0.113.10', 'взят адрес самого узла — вся выборка бессмысленна');
  });

  it('различает протоколы', () => {
    const flows = parseConntrack(CONNTRACK, OWNERS);

    assert.deepEqual(flows.map((f) => f.protocol), ['tcp', 'udp']);
  });

  it('пустая карта владельцев даёт пустую выборку', () => {
    assert.deepEqual(parseConntrack(CONNTRACK, new Map()), []);
  });

  it('мусор в таблице не роняет разбор', () => {
    const junk = ['', 'ipv4 2 icmp 1 29 type=8', 'src=нет-остального', 'ipv4 2 tcp 6 1 src=100.64.0.2'].join('\n');

    assert.deepEqual(parseConntrack(junk, OWNERS), [], 'неполная строка принята за соединение');
  });
});

/**
 * ВЫВОД `conntrack -L` — второй формат той же таблицы (2026-09-20).
 *
 * `/proc/net/nf_conntrack` существует только при `CONFIG_NF_CONNTRACK_PROCFS`, а в современных
 * сборках ядра эту опцию выключают: proc-интерфейс объявлен устаревшим. На нашем узле так и
 * вышло — модули загружены, таблица ведётся, файла нет. Наблюдение при этом молча отдавало ноль
 * соединений, и сигнал формы трафика AWG не мог сработать ни разу.
 *
 * Отличие форматов ровно одно: proc начинает строку с семейства адресов (`ipv4 2 tcp 6 …`),
 * утилита — сразу с протокола (`tcp 6 …`). Всё остальное совпадает, и разбор обязан переварить оба.
 */
const CONNTRACK_CLI = [
  'tcp      6 431999 ESTABLISHED src=100.64.0.2 dst=149.154.167.41 sport=51234 dport=443 src=149.154.167.41 dst=203.0.113.10 sport=443 dport=51234 [ASSURED] mark=0 use=1',
  'udp      17 29 src=100.64.0.2 dst=198.51.100.7 sport=6881 dport=6881 src=198.51.100.7 dst=203.0.113.10 sport=6881 dport=6881 mark=0 use=1',
].join('\n');

describe('разбор вывода утилиты conntrack', () => {
  it('понимает формат без колонки семейства адресов', () => {
    const flows = parseConntrack(CONNTRACK_CLI, OWNERS);

    assert.equal(flows.length, 2, 'записи утилиты не разобрались');
    assert.equal(flows[0]!.destinationIp, '149.154.167.41');
    assert.equal(flows[0]!.destinationPort, 443);
  });

  /**
   * Протокол ищется по строке, а не берётся третьей колонкой: на выводе утилиты третья колонка —
   * это таймаут соединения, и разбор вернул бы число вместо имени. Доля «высоких портов» считается
   * по этим записям, так что мусор в протоколе портит сам сигнал, а не только отчёт.
   */
  it('берёт имя протокола, а не соседнюю колонку', () => {
    const [tcp, udp] = parseConntrack(CONNTRACK_CLI, OWNERS);

    assert.equal(tcp!.protocol, 'tcp');
    assert.equal(udp!.protocol, 'udp');
  });

  it('по-прежнему понимает формат /proc', () => {
    // Запасной путь не должен ломать основной: на ядрах с procfs читается по-прежнему он.
    const [first] = parseConntrack(CONNTRACK, OWNERS);

    assert.equal(first!.protocol, 'tcp');
    assert.equal(first!.destinationIp, '149.154.167.41');
  });
});
