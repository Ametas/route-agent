import fs from 'node:fs/promises';
import path from 'node:path';
import crypto from 'node:crypto';
import pino from 'pino';
import { config } from '../config.js';

const logger = pino({ level: 'info' });

/**
 * Файл rule-set'а со списком задушенных сетевых префиксов (2026-08-30).
 *
 * **Почему локальный файл, а не удалённый rule-set по URL.** `RemoteRuleSet.StartContext` в
 * sing-box возвращает ошибку, если первая загрузка не удалась и в кэше пусто, а ошибка оттуда
 * роняет старт целиком. Оркестратор стал бы обязательной зависимостью КАЖДОЙ ноды в момент её
 * запуска — один его простой превратился бы в простой всего флота. `LocalRuleSet` при этом следит
 * за файлом через `fswatch` и перечитывает его сам, то есть горячая замена сохраняется, а
 * зависимости нет. Доставка идёт по уже аутентифицированному каналу gRPC вместо открытой HTTP-
 * загрузки, и список задушенных сетей не оказывается там, где его прочитает тот, кого душат.
 *
 * **Путь принадлежит агенту, а не конфигу.** Конфиг приходит от оркестратора и ему доверяют, но
 * создавать файл по произвольному пути из присланного документа — плохая привычка: одна опечатка
 * в генераторе, и агент пишет куда-то в систему. Обе стороны просто знают одно и то же имя, ровно
 * как знают общий `proto/agent.proto`.
 */

const RULE_SET_FILE_NAME = 'throttled-ranges.json';

/**
 * Заполнитель для пустого списка. ОБЯЗАН совпадать по смыслу с тем, что собирает оркестратор
 * (`zones/anomaly-traffic/services/throttledRanges.ts`): sing-box считает набор без правил
 * невалидным, поэтому «никого не душим» приходится выражать правилом, которое заведомо ни с чем не
 * совпадёт. `192.0.2.1/32` — TEST-NET-1 из RFC 5737, не маршрутизируется. Намеренно НЕ петля: на
 * 127.0.0.1 приходят локальные инбаунды диспетчера.
 */
const EMPTY_RULE_SET = { version: 1, rules: [{ source_ip_cidr: ['192.0.2.1/32'] }] };

export function throttledRuleSetPath(): string {
  return path.join(path.dirname(config.SINGBOX_CONFIG_PATH), RULE_SET_FILE_NAME);
}

/**
 * Создаёт файл с заполнителем, если его ещё нет.
 *
 * Вызывается ПЕРЕД проверкой конфига в ApplyConfig, и это принципиально. `NewLocalRuleSet` читает
 * файл при создании и возвращает ошибку, если его нет, — а значит `sing-box check` не проходит,
 * `atomicApplyAndReload` откатывается, и нода молча остаётся на старом конфиге. Не падение, но
 * ровно та же тихая беда, что была с `v2ray_api`: пуши не применяются, и никто об этом не узнаёт.
 *
 * Порядком деплоя это не лечится: иначе файл появился бы только при первом вердикте о шейпинге,
 * которого может не случиться никогда.
 */
export async function ensureThrottledRuleSetFile(): Promise<void> {
  const filePath = throttledRuleSetPath();
  try {
    await fs.access(filePath);
    return;
  } catch {
    // Нет файла — создаём.
  }

  try {
    await fs.mkdir(path.dirname(filePath), { recursive: true });
    await fs.writeFile(filePath, `${JSON.stringify(EMPTY_RULE_SET, null, 2)}\n`, 'utf-8');
    logger.info({ filePath }, 'Created an empty throttled-ranges rule-set file');
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    // Не бросаем: пусть провал вылезет понятной ошибкой валидации конфига, а не необъяснимым
    // исключением из середины ApplyConfig.
    logger.error({ err: msg, filePath }, 'Failed to create the throttled-ranges rule-set file');
  }
}

/**
 * Записывает новое содержимое, если оно отличается от текущего.
 *
 * Сравнение обязательно: за файлом следит `fswatch`, и он перечитывает правила на КАЖДУЮ запись.
 * Переписывать файл тем же содержимым значило бы дёргать перезагрузку правил на каждой доставке.
 *
 * Запись атомарная — во временный файл и переименование. `fswatch` иначе мог бы поймать момент,
 * когда файл уже открыт на запись, но ещё пуст, и sing-box прочитал бы невалидный набор.
 */
export async function writeThrottledRuleSet(contentJson: string): Promise<{ changed: boolean }> {
  // Разбираем и пересобираем: так на диск заведомо не попадёт то, что sing-box не прочитает, и
  // сравнение не зависит от форматирования на стороне отправителя.
  const parsed = JSON.parse(contentJson) as { rules?: unknown[] };
  if (!Array.isArray(parsed.rules) || parsed.rules.length === 0) {
    throw new Error('rule-set must contain at least one rule — sing-box rejects an empty set');
  }
  const normalized = `${JSON.stringify(parsed, null, 2)}\n`;

  const filePath = throttledRuleSetPath();
  try {
    const current = await fs.readFile(filePath, 'utf-8');
    if (current === normalized) return { changed: false };
  } catch {
    // Файла нет или он не читается — просто пишем.
  }

  const tempPath = path.join(
    path.dirname(filePath),
    `.${RULE_SET_FILE_NAME}.${Date.now()}_${crypto.randomUUID().slice(0, 8)}.tmp`
  );
  await fs.mkdir(path.dirname(filePath), { recursive: true });
  await fs.writeFile(tempPath, normalized, 'utf-8');
  await fs.rename(tempPath, filePath);
  return { changed: true };
}
