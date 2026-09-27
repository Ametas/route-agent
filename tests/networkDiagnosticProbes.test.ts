import test from 'node:test';
import assert from 'node:assert';
import { buildMtrArgs, normalizeProbe, parseMtrJsonReport } from '../src/services/networkDiagnostic.service.js';

/**
 * Пробы проверки нового сервера (2026-09-27): режим mtr по протоколу и место обрыва пути.
 * Сам mtr в песочнице не запускается — проверяется то, что уходит ему в аргументы, и разбор отчёта.
 */

test('buildMtrArgs: icmp — без режима, tcp/udp — режим и порт перед целью', () => {
  const base = ['--report', '--json', '--no-dns', '--report-cycles', '10'];
  assert.deepStrictEqual(buildMtrArgs({ target: '1.2.3.4', protocol: 'icmp', port: 0 }), [...base, '1.2.3.4']);
  assert.deepStrictEqual(buildMtrArgs({ target: '1.2.3.4', protocol: 'tcp', port: 443 }), [...base, '--tcp', '--port', '443', '1.2.3.4']);
  assert.deepStrictEqual(buildMtrArgs({ target: '1.2.3.4', protocol: 'udp', port: 51821 }), [...base, '--udp', '--port', '51821', '1.2.3.4']);
});

test('normalizeProbe: принимает протокол из списка, нормализует регистр и обнуляет порт у icmp', () => {
  assert.deepStrictEqual(normalizeProbe({ target: ' 1.2.3.4 ', protocol: 'TCP', port: 22 }), {
    ok: true,
    probe: { target: '1.2.3.4', protocol: 'tcp', port: 22 }
  });
  assert.deepStrictEqual(normalizeProbe({ target: '1.2.3.4', protocol: 'icmp', port: 80 }), {
    ok: true,
    probe: { target: '1.2.3.4', protocol: 'icmp', port: 0 }
  });
});

test('normalizeProbe: отвергает чужой протокол, tcp/udp без порта и небезопасную цель', () => {
  const sctp = normalizeProbe({ target: '1.2.3.4', protocol: 'sctp', port: 1 });
  assert.strictEqual(sctp.ok, false);
  assert.match(sctp.ok ? '' : sctp.error, /Unsupported probe protocol/);

  const noPort = normalizeProbe({ target: '1.2.3.4', protocol: 'udp', port: 0 });
  assert.strictEqual(noPort.ok, false);
  assert.match(noPort.ok ? '' : noPort.error, /port must be 1-65535/);

  const tooBig = normalizeProbe({ target: '1.2.3.4', protocol: 'tcp', port: 70000 });
  assert.strictEqual(tooBig.ok, false);

  const flag = normalizeProbe({ target: '-oPonFire', protocol: 'tcp', port: 22 });
  assert.strictEqual(flag.ok, false);
  assert.match(flag.ok ? '' : flag.error, /Invalid or unsafe target/);
});

test('parseMtrJsonReport: последний ответивший хоп — место обрыва у недостижимой цели', () => {
  const report = JSON.stringify({
    report: {
      hubs: [
        { count: 1, host: '10.0.0.1', 'Loss%': 0, Avg: 0.4 },
        { count: 2, host: '188.1.1.1', 'Loss%': 10, Avg: 3.1 },
        { count: 3, host: '???', 'Loss%': 100, Avg: 0 },
        { count: 4, host: '???', 'Loss%': 100, Avg: 0 }
      ]
    }
  });
  const result = parseMtrJsonReport(report);
  assert.strictEqual(result.reachable, false);
  assert.strictEqual(result.lastHopNumber, 2);
  assert.strictEqual(result.lastHopAddress, '188.1.1.1');
});

test('parseMtrJsonReport: у достижимой цели последний ответивший хоп — сама цель', () => {
  const report = JSON.stringify({
    report: {
      hubs: [
        { count: 1, host: '10.0.0.1', 'Loss%': 0, Avg: 0.4 },
        { count: 2, host: '5.6.7.8', 'Loss%': 0, Avg: 21 }
      ]
    }
  });
  const result = parseMtrJsonReport(report);
  assert.strictEqual(result.reachable, true);
  assert.strictEqual(result.lastHopNumber, 2);
  assert.strictEqual(result.lastHopAddress, '5.6.7.8');
});
