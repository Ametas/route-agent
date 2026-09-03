import type { ClashApiEndpoint } from './singboxConnections.js';
import { parseWarpKeyHealth } from './warpKeyHealth.js';

/**
 * Сторож WARP-ветки тыла: уводит звёздный трафик в `direct`, когда живых ключей не осталось, и
 * возвращает обратно, когда они появились.
 *
 * ЗАЧЕМ ЭТО СУЩЕСТВУЕТ. У балансировщика xray, из которого WARP переезжает, был `fallbackTag`:
 * отвалился WARP целиком — трафик сам уходит напрямую. У `urltest` в sing-box такого нет вовсе:
 * при отсутствии истории замеров его `Select` возвращает первого попавшегося участника, то есть
 * трафик уходит в мёртвый туннель. Без этого сторожа переезд потерял бы защиту, которая была.
 *
 * ПОЧЕМУ НА АГЕНТЕ, А НЕ В ОРКЕСТРАТОРЕ. Оркестратор опрашивает здоровье раз в час и списывает
 * ключ лишь со второго промаха подряд — для полного отказа WARP это часы в мёртвом туннеле. И
 * главное: отказ WARP во время недоступности самого оркестратора — как раз тот случай, когда
 * фолбек нужен, а спросить некого. Решение локальное, дешёвое (loopback) и ни от кого не зависит.
 *
 * ЧТО ЭТО НЕ ДЕЛАЕТ: не списывает ключи и не меняет конфиг. Только выбор в селекторе, то есть
 * состояние в памяти sing-box. Списание — дело оркестратора, у него есть база; после перезагрузки
 * конфига селектор возвращается к своему `default`, и сторож оценивает состояние заново.
 */

/** Тег селектора и его членов — должны совпадать с генератором конфига в оркестраторе. */
export const WARP_SELECTOR_TAG = 'warp';
export const WARP_POOL_TAG = 'wg-pool';
export const DIRECT_TAG = 'direct';

export type WarpSelection = typeof WARP_POOL_TAG | typeof DIRECT_TAG;

export interface GuardDecision {
  /** Что должно быть выбрано по текущему состоянию ключей. */
  desired: WarpSelection;
  /** Сколько ключей ответило на последнем замере sing-box. */
  aliveKeys: number;
  totalKeys: number;
}

/**
 * Решение по срезу `/proxies`.
 *
 * `null` означает «не вмешиваться»: селектора в конфиге нет вовсе. Так выглядит нода, где WARP не
 * включён или пул ещё пуст (генератор тогда не создаёт ни группу, ни селектор), и трогать там
 * нечего.
 */
export function decideWarpSelection(payload: unknown): GuardDecision | null {
  const proxies = (payload as { proxies?: Record<string, unknown> })?.proxies;
  if (!proxies || typeof proxies !== 'object' || !(WARP_SELECTOR_TAG in proxies)) return null;

  // Живость считаем по тем же правилам, что и отчёт о здоровье: пустая история — это «не ответил»,
  // потому что sing-box УДАЛЯЕТ запись при неудачном замере и пишет при удачном.
  const keys = parseWarpKeyHealth(payload);
  const aliveKeys = keys.filter((key) => key.alive).length;

  return {
    desired: aliveKeys > 0 ? WARP_POOL_TAG : DIRECT_TAG,
    aliveKeys,
    totalKeys: keys.length,
  };
}

/** Что выбрано сейчас. `null` — селектор не найден или ответ не той формы. */
export function currentSelection(payload: unknown): string | null {
  const proxies = (payload as { proxies?: Record<string, { now?: unknown }> })?.proxies;
  const selector = proxies?.[WARP_SELECTOR_TAG];
  return selector && typeof selector.now === 'string' ? selector.now : null;
}

function authHeaders(endpoint: ClashApiEndpoint): Record<string, string> {
  return endpoint.secret ? { authorization: `Bearer ${endpoint.secret}` } : {};
}

/**
 * Переключает селектор.
 *
 * Форма запроса сверена по исходникам sing-box: `PUT /proxies/{name}` с телом `{"name": "<член>"}`,
 * успех — 204 без тела. Цель обязана быть селектором (иначе 400 «Must be a Selector»), а член —
 * входить в группу (иначе 400 «not found»).
 */
export async function selectWarpMember(endpoint: ClashApiEndpoint, member: WarpSelection): Promise<void> {
  const response = await fetch(`http://${endpoint.address}/proxies/${WARP_SELECTOR_TAG}`, {
    method: 'PUT',
    headers: { ...authHeaders(endpoint), 'content-type': 'application/json' },
    body: JSON.stringify({ name: member }),
    signal: AbortSignal.timeout(5000),
  });
  if (!response.ok) {
    throw new Error(`clash api responded ${response.status} selecting ${member}`);
  }
}
