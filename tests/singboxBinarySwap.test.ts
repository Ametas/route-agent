import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

/**
 * ПОДМЕНА БИНАРНИКА SING-BOX ОБЯЗАНА ЗАКАНЧИВАТЬСЯ RESTART (2026-09-20, два фронта подряд).
 *
 * Симптом одинаковый и полностью беззвучный: бинарь на диске новый, `sing-box version` и
 * телеметрия рапортуют новую версию, а в работе остаётся процесс, поднятый прежним файлом. Дальше
 * агент, увидев форк НА ДИСКЕ, подмешивает в конфиг службу `users-api`, старое ядро отвечает
 * `unknown inbound type: users-api` и продолжает работать на прежнем конфиге. Узел при этом
 * зелёный во всех проверках.
 *
 * Привести к этому могут ДВА разных вызова, и первый заход починил только один из них:
 *
 *   `systemctl reload`     — SIGHUP запущенному процессу: перечитывает конфиг, образ не меняет;
 *   `systemctl enable --now` — для уже активного юнита пустая операция.
 *
 * Отсюда проверка по исходнику, а не по поведению: под тестовым окружением эта ветка целиком
 * пропускается (`NODE_ENV !== 'test'`), а утверждение всё равно структурное — оно про то, КАКОЙ
 * командой заканчивается подмена файла.
 */

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');

function uploadHandlerSource(): string {
  const src = readFileSync(resolve(root, 'src/services/binary.service.ts'), 'utf8').replace(/\r\n/g, '\n');
  const start = src.indexOf('export async function uploadSingboxBinaryHandler');
  assert.notEqual(start, -1, 'обработчик загрузки sing-box пропал — обнови тест вместе с ним');
  const end = src.indexOf('\nexport ', start + 1);
  return src.slice(start, end === -1 ? undefined : end);
}

describe('подмена бинарника sing-box', () => {
  it('заканчивается перезапуском юнита', () => {
    assert.match(uploadHandlerSource(), /execAsync\(config\.SINGBOX_RESTART_COMMAND\)/);
  });

  it('перезапуск не спрятан в ветку — он выполняется при любом состоянии юнита', () => {
    /**
     * Ровно то, на чём поймал второй фронт: перезапуск стоял в `else`, а обновление файла юнита
     * уводило исполнение в соседнюю ветку с `enable --now`, которая активному юниту ничего не
     * делает. У операции «подменили бинарь» один правильный исход, и он не должен зависеть от
     * того, тронули ли заодно файл юнита.
     */
    const body = uploadHandlerSource();
    const restartAt = body.indexOf('config.SINGBOX_RESTART_COMMAND');
    const branchAt = body.indexOf('if (unitChanged)');

    assert.ok(branchAt !== -1, 'ветка провижининга юнита исчезла — обнови тест');
    assert.ok(restartAt > branchAt, 'перезапуск снова оказался внутри развилки');
    assert.doesNotMatch(
      body.slice(branchAt, restartAt),
      /\belse\b/,
      'перезапуск снова стал альтернативой провижинингу, а не безусловным шагом'
    );
  });

  it('не пытается ввести новый бинарь в работу через reload или enable --now', () => {
    /**
     * Ищем ФОРМУ ВЫЗОВА, а не упоминание команды: оба промаха разобраны прямо здесь в
     * комментариях, и проверка по подстроке краснела бы на собственном объяснении. На этом я уже
     * спотыкался в соседнем репозитории — слишком широкий шаблон ловит прозу.
     */
    const body = uploadHandlerSource();

    assert.doesNotMatch(body, /execAsync\(config\.RELOAD_COMMAND/, 'reload оставляет запущенным старый образ');
    assert.doesNotMatch(body, /execAsync\(['"`]systemctl enable --now/, 'для активного юнита это пустая операция');
  });
});
