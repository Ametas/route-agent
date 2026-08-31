import { readClashApiEndpoint, fetchConnections, type SingBoxConnectionRecord } from './singboxConnections.js';

/**
 * Пер-юзерные счётчики трафика sing-box поверх Clash API (2026-09-01).
 *
 * **Что здесь было раньше и почему это молча сломалось.** Модуль ходил в `experimental.v2ray_api`
 * по gRPC. Оркестратор перестал эмитить этот блок (официальные сборки sing-box собираются без
 * `with_v2ray_api`, и конфиг с ним не проходит даже `sing-box check`), но эта половина за ним не
 * пошла: она продолжала искать в применённом конфиге `experimental.v2ray_api.listen`, не находила
 * и отвечала «не настроено» — успешным ответом с пустым списком. То есть оркестратор каждые три
 * минуты видел успешный проход, в котором ни у кого нет трафика. Ни ошибки, ни алерта. Подсистема
 * выглядела рабочей и не работала.
 *
 * **Почему дельту приходится считать здесь.** v2ray_api отдавал приращение — его счётчики
 * обнулялись при чтении. Оркестратор на это опирается: он берёт присланное число как трафик за
 * ОДИН тик свипа и сравнивает с порогом всплеска. Clash API так не умеет, он отдаёт абсолютные
 * байты по каждому живому и недавно закрытому соединению. Отдать их как есть значило бы, что порог
 * всплеска начнёт срабатывать на накопленной сумме, а не на скорости — то есть на любом
 * пользователе, который просто долго сидит. Поэтому приращение считается тут, а контракт RPC
 * остаётся прежним.
 */

export interface SingBoxUserTrafficDelta {
  userUuid: string;
  uplinkBytes: number;
  downlinkBytes: number;
}

/** Абсолютные счётчики одного соединения на момент прошлого прохода. */
interface ConnectionTotals {
  upload: number;
  download: number;
}

/**
 * Предохранитель от роста памяти. На живой ноде число соединений в двух наборах Clash API
 * (живые + буфер завершённых) на порядки меньше, так что это не рабочий предел, а потолок на
 * случай, если что-то пойдёт не так.
 */
const MAX_TRACKED_CONNECTIONS = 100_000;

let lastTotals = new Map<string, ConnectionTotals>();

/**
 * Был ли уже хоть один проход. До него базовой линии нет: соединения, открытые ДО старта агента,
 * приходят с уже накопленными байтами, и посчитать их как приращение за один тик значило бы
 * отрапортовать гигабайты за три минуты — ровно ложный всплеск на ровном месте.
 */
let primed = false;

/**
 * Считает приращение по каждому пользователю между двумя проходами.
 *
 * Чистая функция ради тестов: вся логика подсчёта здесь, вокруг остаётся только сеть.
 *
 * @param previous абсолютные счётчики с прошлого прохода
 * @param isFirstPass на первом проходе возвращает нулевые дельты и только запоминает базовую линию
 */
export function computeUserTrafficDeltas(
  records: SingBoxConnectionRecord[],
  previous: Map<string, ConnectionTotals>,
  isFirstPass: boolean
): { deltas: SingBoxUserTrafficDelta[]; next: Map<string, ConnectionTotals> } {
  const next = new Map<string, ConnectionTotals>();
  const perUser = new Map<string, { uplinkBytes: number; downlinkBytes: number }>();

  for (const record of records) {
    // Без идентификатора нельзя связать запись с прошлым проходом, без пользователя — не к кому
    // отнести трафик. И то, и другое — отбраковка, а не повод считать с нуля: посчитать с нуля
    // здесь означало бы каждый проход заново записывать всю историю соединения как приращение.
    if (record.id === '' || record.user === '') continue;

    // Запоминаем ВСЁ, что видели в этом проходе, включая завершённые. Забывать закрытое сразу
    // нельзя: Clash отдаёт буфер завершённых несколько проходов подряд, и забытое соединение на
    // следующем проходе выглядело бы новым — его полные байты посчитались бы ещё раз. Записи,
    // которых в этом проходе не было, просто не попадают в `next` и тем самым отпускаются.
    if (next.size < MAX_TRACKED_CONNECTIONS) {
      next.set(record.id, { upload: record.uploadBytes, download: record.downloadBytes });
    }

    if (isFirstPass) continue;

    const prev = previous.get(record.id);
    // Незнакомое соединение — целиком новое, его байты и есть приращение.
    const prevUpload = prev?.upload ?? 0;
    const prevDownload = prev?.download ?? 0;

    // Отрицательного приращения быть не должно, но если счётчик уехал вниз (перезапуск sing-box с
    // переиспользованным идентификатором), ноль честнее отрицательного числа, которое оркестратор
    // вычтет из дневного накопителя.
    const up = Math.max(0, record.uploadBytes - prevUpload);
    const down = Math.max(0, record.downloadBytes - prevDownload);
    if (up === 0 && down === 0) continue;

    const acc = perUser.get(record.user) ?? { uplinkBytes: 0, downlinkBytes: 0 };
    acc.uplinkBytes += up;
    acc.downlinkBytes += down;
    perUser.set(record.user, acc);
  }

  const deltas = Array.from(perUser, ([userUuid, bytes]) => ({ userUuid, ...bytes }));
  return { deltas, next };
}

/** Сбрасывает накопленное состояние. Только для тестов — в рантайме звать нечего. */
export function resetTrafficDeltaState(): void {
  lastTotals = new Map();
  primed = false;
}

/**
 * Приращение трафика по пользователям с прошлого вызова.
 *
 * Возвращает `null`, когда Clash API на ноде не настроен — это не ошибка, а нормальное состояние
 * ноды, на которую ещё не приезжал конфиг. Вызывающий отличает этот случай от пустого списка сам:
 * `null` — «спросить негде», `[]` — «спросили, приращения нет».
 */
export async function queryUserTrafficDeltas(): Promise<SingBoxUserTrafficDelta[] | null> {
  const endpoint = await readClashApiEndpoint();
  if (!endpoint) return null;

  const records = await fetchConnections(endpoint);
  const { deltas, next } = computeUserTrafficDeltas(records, lastTotals, !primed);

  lastTotals = next;
  primed = true;

  return deltas;
}
