import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { parseMihomoConnections } from '../src/utils/rearConnections.js';

/**
 * Разбор соединений тылового ядра.
 *
 * ФОРМА СНЯТА С БОЕВОГО УЗЛА, а не выведена из документации. Это принципиально: на выдуманных
 * именах полей мы уже обожглись — `meta.user` и `meta.inboundTag` у sing-box читались с самого
 * начала и НЕ СУЩЕСТВУЮТ (проверено на живом узле: 116 соединений, включая 18 через tuic-инбаунды
 * с заведёнными пользователями, — поля `user` нет ни в одном).
 *
 * ЗАЧЕМ ВООБЩЕ ТЫЛ. Он единственный видит настоящее внешнее назначение: у фронта назначение — это
 * кольцо, у эгресса — петля на локальный порт. Тыл по реальному адресу маршрутизирует, значит
 * знает его точно.
 */

/** Настоящий ответ mihomo, снятый с узла 2026-09-17 (сокращён до читаемого). */
const LIVE = {
  downloadTotal: 265401168140,
  uploadTotal: 18306334262,
  connections: [
    {
      id: '9429b024-9fd1-48ea-a64b-1cfcce8a08b6',
      upload: 14890,
      download: 15444,
      start: '2026-09-17T05:33:12.378987889Z',
      chains: ['DIRECT'],
      metadata: {
        network: 'udp',
        type: 'Vless',
        sourceIP: '127.0.0.1',
        destinationIP: '172.217.118.4',
        sourcePort: '34184',
        destinationPort: '443',
        inboundIP: '127.0.0.1',
        inboundPort: '29000',
        inboundName: 'stars-in',
        inboundUser: 'link',
        host: '',
        sniffHost: '',
        dnsMode: 'normal',
      },
    },
  ],
};

describe('соединения тыла', () => {
  it('читает назначение, порт и байты по направлениям', () => {
    const [record] = parseMihomoConnections(LIVE);

    assert.equal(record!.destinationIp, '172.217.118.4');
    assert.equal(record!.destinationPort, 443);
    assert.equal(record!.uploadBytes, 14890);
    assert.equal(record!.downloadBytes, 15444);
  });

  /**
   * ЭТО ТО, ЧЕГО НЕТ НИГДЕ БОЛЬШЕ. У sing-box `inbound_tag` всегда пуст, и разделить звёздный
   * трафик от игрового по его записям невозможно. Тыл называет инбаунд прямо.
   */
  it('заполняет тег инбаунда, который у sing-box всегда пуст', () => {
    assert.equal(parseMihomoConnections(LIVE)[0]!.inboundTag, 'stars-in');
  });

  /**
   * `inboundUser` переносится КАК ЕСТЬ, хотя он одинаков у всех абонентов (`link` — общий uuid
   * связки). Подменять его пустой строкой значило бы скрыть, что поле вообще заполняется, — а
   * именно из него вырастет пер-абонентская привязка, если её когда-нибудь заведут.
   */
  it('переносит имя пользователя связки, не выдавая его за абонента', () => {
    assert.equal(parseMihomoConnections(LIVE)[0]!.user, 'link');
  });

  /** Порт у mihomo приходит строкой, байты числом — приводим обе формы. */
  it('строковый порт приводится к числу', () => {
    assert.strictEqual(typeof parseMihomoConnections(LIVE)[0]!.destinationPort, 'number');
  });

  it('время старта разбирается в миллисекунды', () => {
    assert.equal(parseMihomoConnections(LIVE)[0]!.startedAtUnixMs, Date.parse('2026-09-17T05:33:12.378987889Z'));
  });

  /** `host` — что попросил клиент, `sniffHost` — что вынюхало ядро. Первое точнее, когда есть. */
  it('домен берётся из host, а при его отсутствии из sniffHost', () => {
    const asked = parseMihomoConnections({ connections: [{ metadata: { host: 'youtube.com', sniffHost: 'x.com' } }] });
    const sniffed = parseMihomoConnections({ connections: [{ metadata: { host: '', sniffHost: 'x.com' } }] });

    assert.equal(asked[0]!.destinationDomain, 'youtube.com');
    assert.equal(sniffed[0]!.destinationDomain, 'x.com');
  });

  it('мусор в ответе не роняет разбор', () => {
    for (const junk of [null, {}, { connections: null }, { connections: 'нет' }]) {
      assert.deepEqual(parseMihomoConnections(junk), [], `упало на ${JSON.stringify(junk)}`);
    }
  });

  it('запись без метаданных не додумывается', () => {
    const [record] = parseMihomoConnections({ connections: [{ id: 'x' }] });

    assert.equal(record!.destinationIp, '');
    assert.equal(record!.uploadBytes, 0);
  });
});
