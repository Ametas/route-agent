import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { extractUdpTunnelPorts, planFirewallSync } from '../src/utils/firewall.js';

/**
 * ПЕРЕЗАГРУЗКА ФАЕРВОЛА ТОЛЬКО КОГДА ЕСТЬ ЧТО ПРИМЕНЯТЬ (аудит 2026-09-21).
 *
 * `syncEgressFirewall` вызывается на КАЖДОМ применении конфига, а `sudo ufw reload` в её конце
 * выполнялся безусловно — даже когда открывать и закрывать нечего. Портовый состав от появления
 * абонента не меняется никогда, значит каждое создание пользователя заставляло весь флот
 * перестраивать правила iptables впустую. В журнале узла это видно как `sudo ufw reload` при
 * обычном пуше конфига.
 *
 * Проверяется решение, а не его исполнение: сама функция ходит в `sudo` и в файл по абсолютному
 * пути, и проверить её можно только на живой машине — а ошибка была именно в правиле.
 */

describe('план синхронизации фаервола', () => {
  it('без изменений не требует перезагрузки', () => {
    const plan = planFirewallSync([443, 8443], [443, 8443]);

    assert.deepEqual(plan.toOpen, []);
    assert.deepEqual(plan.toClose, []);
    assert.strictEqual(plan.needsReload, false, 'перезагрузка правил без единого изменения');
  });

  it('порядок портов не считается изменением', () => {
    // Набор приходит из разбора конфига, и порядок инбаундов в нём не закреплён ничем.
    const plan = planFirewallSync([8443, 443], [443, 8443]);

    assert.strictEqual(plan.needsReload, false, `порядок принят за изменение: ${JSON.stringify(plan)}`);
  });

  it('новый порт открывается и требует перезагрузки', () => {
    const plan = planFirewallSync([443, 8443], [443]);

    assert.deepEqual(plan.toOpen, [8443]);
    assert.deepEqual(plan.toClose, []);
    assert.strictEqual(plan.needsReload, true);
  });

  it('исчезнувший порт закрывается и требует перезагрузки', () => {
    const plan = planFirewallSync([443], [443, 8443]);

    assert.deepEqual(plan.toOpen, []);
    assert.deepEqual(plan.toClose, [8443]);
    assert.strictEqual(plan.needsReload, true);
  });

  it('пустой кэш означает открыть всё — самолечение после потери файла', () => {
    /**
     * Кэш лежит файлом на узле и может пропасть: переустановка агента, чистка диска. Тогда
     * «прошлых портов» нет, и правила надо выставить заново — пропустить этот случай значило бы
     * оставить узел без открытых портов до следующей смены конфига.
     */
    const plan = planFirewallSync([443, 8443], []);

    assert.deepEqual(plan.toOpen, [443, 8443]);
    assert.strictEqual(plan.needsReload, true);
  });

  it('конфиг без UDP-инбаундов закрывает всё, что было', () => {
    const plan = planFirewallSync([], [443, 8443]);

    assert.deepEqual(plan.toClose, [443, 8443]);
    assert.strictEqual(plan.needsReload, true);
  });

  it('пусто с обеих сторон — тоже без перезагрузки', () => {
    assert.strictEqual(planFirewallSync([], []).needsReload, false);
  });
});

/**
 * 2026-09-28: TUIC и Hysteria2 слушают 127.0.0.1 за диспетчером на 443, и их порты открывались
 * в ufw зря. Открывается только то, что слушает наружу.
 */
describe('порты UDP-инбаундов для фаервола', () => {
  it('пропускает инбаунды на loopback и берёт слушающие наружу', () => {
    const ports = extractUdpTunnelPorts({
      inbounds: [
        { type: 'direct', tag: 'quic-dispatch', listen: '0.0.0.0', listen_port: 443 },
        { type: 'tuic', listen: '127.0.0.1', listen_port: 20001 },
        { type: 'hysteria2', listen: '::1', listen_port: 20002 },
        { type: 'hysteria2', listen: '0.0.0.0', listen_port: 2053 },
        { type: 'tuic', listen_port: 40013 },
      ],
    });
    assert.deepEqual(ports.sort((a, b) => a - b), [2053, 40013]);
  });

  it('открытые раньше порты loopback-инбаундов уходят в закрытие', () => {
    const plan = planFirewallSync([], [20001, 20002]);
    assert.deepEqual(plan.toClose, [20001, 20002]);
    assert.equal(plan.needsReload, true);
  });
});
