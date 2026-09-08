import pino from 'pino';
import { readActiveRearClashApi } from './rearCore.js';
import type { JournalWarning } from './journalWarnings.js';

const logger = pino({ level: 'info' });

/**
 * «Правило есть, а не ловит» — набор правил, загруженный ПУСТЫМ.
 *
 * ЧТО СЛУЧИЛОСЬ (germany-node, 2026-09-08). Оркестратор объявил в конфиге тыла провайдер
 * `ip-telegram`, а файл на узел не довёз — наборы возятся отдельным шагом, и состав разошёлся.
 * mihomo при этом НЕ УПАЛ: он загрузил провайдер пустым.
 *
 *   "name":"ip-telegram","ruleCount":0
 *
 * Правило `RULE-SET,ip-telegram,warp,no-resolve` перестало ловить что-либо, и весь MTProto-трафик
 * телеграма пошёл напрямую вместо WARP. Ни ошибки, ни строчки в журнале — единственный след
 * `chains:["DIRECT"]` в API самого тыла, куда никто не смотрит.
 *
 * ПОЧЕМУ ЭТО НЕЛЬЗЯ ЗАКРЫТЬ НА СТОРОНЕ ОРКЕСТРАТОРА. Он уже сверяет отпечаток состава и довозит
 * разошедшееся — но это лечит ОДНУ причину пустоты. Родственники дают ту же картину и мимо
 * отпечатка: обрезанная загрузка, битый `.mrs`, набор, который у MetaCubeX опустел, файл, удалённый
 * на узле. Про все них знает только сам тыл, и спросить его может только агент.
 *
 * ПОЧЕМУ ЗДЕСЬ, А НЕ В ЖУРНАЛЬНЫХ ОБРАЗЦАХ. Тот же класс — тихая деградация, — и уезжает тем же
 * RPC `PullNodeWarnings`. Но источник другой: не строка в журнале, а состояние живого процесса.
 * Ровно тот же довод, что у `listenerWarning.ts`: грепом отсутствие события не найти.
 */

/** Стабильный идентификатор класса — по нему оркестратор дедуплицирует доклады. */
export const REAR_RULE_PROVIDER_EMPTY_KIND = 'rear_rule_provider_empty';

interface RuleProvider {
  name?: unknown;
  ruleCount?: unknown;
  behavior?: unknown;
  vehicleType?: unknown;
}

/**
 * Имена провайдеров с нулём правил.
 *
 * ЧИСТАЯ ФУНКЦИЯ ПОВЕРХ ОТВЕТА API — чтобы разбор проверялся без живого тыла и без сети.
 *
 * `ruleCount` отсутствует или не число — НЕ считаем пустым: это чужая форма ответа, а не
 * доказанный ноль. Придумывать тревогу по непонятному полю значило бы учить админа не читать
 * доклады.
 */
export function emptyRuleProviders(payload: unknown): string[] {
  if (typeof payload !== 'object' || payload === null) return [];
  const providers = (payload as { providers?: unknown }).providers;
  if (typeof providers !== 'object' || providers === null) return [];

  const empty: string[] = [];
  for (const [key, raw] of Object.entries(providers as Record<string, unknown>)) {
    if (typeof raw !== 'object' || raw === null) continue;
    const provider = raw as RuleProvider;
    if (typeof provider.ruleCount !== 'number') continue;
    if (provider.ruleCount > 0) continue;

    empty.push(typeof provider.name === 'string' && provider.name.length > 0 ? provider.name : key);
  }

  return empty.sort();
}

/**
 * Спрашивает у работающего тыла состав его наборов и докладывает пустые.
 *
 * `null` — докладывать нечего: тыла нет, он не отвечает, или все наборы полны. Отсутствие тыла не
 * тревога: на узле без WARP его и не должно быть.
 */
export async function detectEmptyRearRuleProviders(): Promise<JournalWarning | null> {
  const api = await readActiveRearClashApi();
  if (!api) return null;

  let payload: unknown;
  try {
    const response = await fetch(`http://${api.address}/providers/rules`, {
      headers: { Authorization: `Bearer ${api.secret}` },
      // Тыл на петле: отвечает мгновенно либо не отвечает вовсе. Долгое ожидание здесь ничего не
      // покупает, а проход по предупреждениям задержало бы.
      signal: AbortSignal.timeout(3000),
    });
    if (!response.ok) return null;
    payload = await response.json();
  } catch {
    /**
     * Молча: недоступный API тыла — это уже другая тревога (служба не поднялась), и её ловят
     * журнальные образцы вместе с проверкой слушателей. Докладывать её и отсюда значило бы
     * дублировать один факт двумя классами.
     */
    return null;
  }

  const empty = emptyRuleProviders(payload);
  if (empty.length === 0) return null;

  logger.warn({ providers: empty }, 'Rear rule providers loaded empty — their rules match nothing');

  return {
    kind: REAR_RULE_PROVIDER_EMPTY_KIND,
    // Источник — ядро тыла: у одного и того же класса на mihomo и на sing-box разные причины.
    source: api.core.label,
    /**
     * В образец идут ИМЕНА, а не число: администратору нужно знать, какое правило замолчало, —
     * «два набора пусты» не даёт ничего, кроме тревоги.
     */
    sample: `наборы без правил: ${empty.join(', ')}`,
    occurrences: empty.length,
    lastSeenUnixMs: Date.now(),
  };
}
