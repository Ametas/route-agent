// tests/rearRuleProviderWarning.test.ts
import { describe, it } from 'node:test';
import assert from 'node:assert/strict';
import { emptyRuleProviders } from '../src/utils/rearRuleProviderWarning.js';

/**
 * Набор правил, загруженный ПУСТЫМ.
 *
 * ЖИВОЙ ОТКАЗ 2026-09-08, germany-node. Оркестратор объявил в конфиге тыла провайдер `ip-telegram`,
 * а файл не довёз — наборы возятся отдельным шагом. mihomo при этом НЕ УПАЛ, он загрузил провайдер
 * пустым:
 *
 *   "name":"ip-telegram","ruleCount":0
 *
 * Правило `RULE-SET,ip-telegram,warp,no-resolve` перестало ловить что-либо, и весь MTProto-трафик
 * телеграма пошёл напрямую вместо WARP. Ни ошибки, ни строчки в журнале — единственный след
 * `chains:["DIRECT"]` в API самого тыла.
 *
 * Разбор вынесен чистой функцией, чтобы проверяться без живого тыла и без сети.
 */

/** Ответ `/providers/rules` в том виде, в каком его отдаёт mihomo. */
function payload(providers: Record<string, { name?: string; ruleCount?: unknown }>): unknown {
  return { providers };
}

describe('пустые наборы правил тыла', () => {
  it('называет провайдер с нулём правил', () => {
    const empty = emptyRuleProviders(
      payload({
        'ip-telegram': { name: 'ip-telegram', ruleCount: 0 },
        'category-media': { name: 'category-media', ruleCount: 1580 },
      })
    );

    assert.deepEqual(empty, ['ip-telegram']);
  });

  it('молчит, когда все наборы полны', () => {
    const empty = emptyRuleProviders(
      payload({
        'category-media': { name: 'category-media', ruleCount: 1580 },
        'category-games': { name: 'category-games', ruleCount: 1123 },
      })
    );

    assert.deepEqual(empty, []);
  });

  /**
   * Отсутствующий или нечисловой `ruleCount` — это ЧУЖАЯ форма ответа, а не доказанный ноль.
   * Придумывать тревогу по непонятному полю значило бы приучить админа не читать доклады.
   */
  it('не считает пустым то, чего не понимает', () => {
    const empty = emptyRuleProviders(
      payload({
        'no-count': { name: 'no-count' },
        'string-count': { name: 'string-count', ruleCount: '0' },
        'null-count': { name: 'null-count', ruleCount: null },
      })
    );

    assert.deepEqual(empty, [], 'непонятная форма поднята как тревога');
  });

  /** Порядок ключей у объекта не гарантирован — доклад не должен от него зависеть. */
  it('имена отсортированы', () => {
    const empty = emptyRuleProviders(
      payload({
        'ip-telegram': { name: 'ip-telegram', ruleCount: 0 },
        'category-ai-!cn': { name: 'category-ai-!cn', ruleCount: 0 },
      })
    );

    assert.deepEqual(empty, ['category-ai-!cn', 'ip-telegram']);
  });

  /** У mihomo имя дублируется ключом; если поля нет — берём ключ, чтобы доклад не остался безымянным. */
  it('падает на ключ, когда поля name нет', () => {
    assert.deepEqual(emptyRuleProviders(payload({ 'ip-telegram': { ruleCount: 0 } })), ['ip-telegram']);
  });

  it('чужая форма ответа не роняет разбор', () => {
    for (const junk of [null, undefined, 42, 'строка', {}, { providers: null }, { providers: [] }]) {
      assert.deepEqual(emptyRuleProviders(junk), [], `упало на ${JSON.stringify(junk)}`);
    }
  });
});
