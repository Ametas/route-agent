import test from 'node:test';
import assert from 'node:assert';
import { promises as fs } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const servicesDir = path.join(__dirname, '..', 'src', 'services');

/**
 * Сторож на конкретный класс ошибки, который уже стоил разбора (2026-09-03).
 *
 * Сервер грузит proto с `keepCase: false`, то есть поля на проводе — camelCase. Обработчик,
 * возвращающий `skipped_reason`, отдаёт поле, которого в схеме нет: proto-loader молча его
 * ВЫБРАСЫВАЕТ — ни ошибки, ни предупреждения, оркестратор просто всегда видит там пустоту.
 * Проверено round-trip'ом через сам загрузчик: `skipped_reason` приезжает как `""`, а
 * `skippedReason` — как отправлено.
 *
 * Так и случилось в `ConfigureRearSingbox`: теста на это поле там не было, и промах уехал бы на
 * прод. Поймал его соседний RPC, где тест был.
 *
 * ЧТО ЗДЕСЬ РАЗРЕШЕНО. Несколько давних обработчиков намеренно кладут ОБА написания рядом
 * (`loss_percent` и `lossPercent`) — приём «на всякий случай», и он работает: значение несёт
 * camelCase-двойник. Такие пары не нарушение, поэтому проверяется именно ОТСУТСТВИЕ двойника, а не
 * наличие snake_case. Первая версия теста этого не различала и подняла ложную тревогу на рабочем
 * коде — проверять пришлось round-trip'ом.
 *
 * Почему по исходникам, а не по поведению: обе ветки `ConfigureRearSingbox`, заполняющие
 * `skippedReason`, в тестовой среде недостижимы — одна требует отсутствующего бинаря sing-box,
 * другая настоящего отказа `sing-box check`, и обе закорочены под NODE_ENV=test. Полноценное
 * покрытие тех веток записано в `.claude/BACKLOG.md`.
 */

/** Канонизация схемы для хеша контракта — там snake_case обязателен и сверяется с оркестратором. */
const EXCLUDED = new Set(['protoContract.service.ts']);

/** Комментарии убираем: иначе тест ловит собственные пояснения (на этом уже обжигались). */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');
}

function toCamel(snake: string): string {
  return snake.replace(/_([a-z0-9])/g, (_, ch: string) => ch.toUpperCase());
}

test('a snake_case response key always has a camelCase twin', async () => {
  const files = (await fs.readdir(servicesDir)).filter((name) => name.endsWith('.service.ts') && !EXCLUDED.has(name));
  assert.ok(files.length > 0, 'не найдено ни одного сервиса — путь к каталогу сломан');

  const offenders: string[] = [];

  /**
   * Двойник должен стоять В ТОМ ЖЕ ЛИТЕРАЛЕ, поэтому ищем в узком окне соседних строк.
   *
   * Первая редакция искала его во всём файле — и мутационный прогон показал, что этого мало:
   * возвращённый в одну ветку `skipped_reason` проходил проверку, потому что в СОСЕДНЕЙ ветке того
   * же обработчика стоял правильный `skippedReason`. Проверка была зелёной на сломанном коде.
   *
   * В настоящем коде пары кладут вплотную, строка в строку, так что окна в пару строк хватает.
   */
  const TWIN_WINDOW = 3;

  for (const file of files) {
    const lines = stripComments(await fs.readFile(path.join(servicesDir, file), 'utf-8')).split('\n');

    lines.forEach((line, index) => {
      const match = /^\s*([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\s*:/.exec(line);
      if (!match) return;

      const snake = match[1];
      const twin = toCamel(snake);
      const window = lines.slice(Math.max(0, index - TWIN_WINDOW), index + TWIN_WINDOW + 1);

      if (!window.some((near) => new RegExp(`^\\s*${twin}\\s*:`).test(near))) {
        offenders.push(`${file}:${index + 1}: ${snake} (рядом нет двойника ${twin})`);
      }
    });
  }

  assert.deepStrictEqual(
    offenders,
    [],
    `snake_case-ключи без camelCase-двойника — proto-loader выбросит их молча:\n${offenders.join('\n')}`
  );
});
