import * as fs from 'fs/promises';
import path from 'path';

/**
 * Набор файлов, которые операция СОБИРАЕТСЯ записать, и проверка «а не лежит ли уже ровно это».
 *
 * ЗАЧЕМ. Пуш конфига Caddy шёл дважды подряд с одинаковым содержимым: шаг бота звал синк явно, а
 * `pushConfigToNode` делал его же пресинком внутри себя (отслежено по логам владельца 2026-09-04 —
 * два одинаковых `ConfigureCaddy` в одну секунду). Второй проход перезаписывал те же файлы,
 * порождал процесс `caddy validate` и ПЕРЕЗАГРУЖАЛ Caddy на живом узле. Сертификаты при этом заново
 * не выпускались — они уже были, — но перезагрузка фронтового Caddy бесплатной не бывает.
 *
 * Лечится не в вызывающем, а здесь: повторный синк с тем же содержимым обязан быть дешёвым, кто бы
 * его ни инициировал. Тот же приём уже применён к конфигу тылового sing-box.
 *
 * СРАВНИВАЕМ ВСЁ, ЧТО ПИШЕМ, а не только главный файл. У `ConfigureCaddy` файлов пять видов:
 * Caddyfile, страница-заглушка, файл с токеном Cloudflare, одиночная пара сертификат/ключ и пачка
 * кольцевых. Сравнить один Caddyfile и пропустить запись значило бы не доставить изменившийся
 * сертификат — то есть заменить лишнюю перезагрузку тихой недоставкой.
 */
export interface PlannedWrite {
  path: string;
  content: string;
  /** Права доступа, если файл обязан быть закрытым (ключи, токены). */
  mode?: number;
}

/**
 * Лежит ли на диске ровно то, что мы собираемся записать.
 *
 * ПРАВА УЧАСТВУЮТ В СРАВНЕНИИ. Файл с верным содержимым, но разъехавшимися правами — это приватный
 * ключ, доступный лишним читателям. Считать такой файл «уже актуальным» значило бы законсервировать
 * проблему: пропуск записи заодно пропустил бы и `chmod`.
 *
 * Пустой список — это «писать нечего», и совпадением он НЕ считается: у вызывающего в таком случае
 * нет оснований думать, что состояние на узле верное.
 */
export async function allWritesAlreadyOnDisk(writes: readonly PlannedWrite[]): Promise<boolean> {
  if (writes.length === 0) return false;

  for (const write of writes) {
    const existing = await fs.readFile(write.path, 'utf-8').catch(() => null);
    if (existing !== write.content) return false;

    if (write.mode !== undefined) {
      const stat = await fs.stat(write.path).catch(() => null);
      if (!stat || (stat.mode & 0o777) !== write.mode) return false;
    }
  }

  return true;
}

/** Выполняет запланированные записи: каталог, файл, права. */
export async function applyPlannedWrites(writes: readonly PlannedWrite[]): Promise<void> {
  for (const write of writes) {
    await fs.mkdir(path.dirname(write.path), { recursive: true });

    if (write.mode === undefined) {
      await fs.writeFile(write.path, write.content, 'utf-8');
      continue;
    }

    await fs.writeFile(write.path, write.content, { encoding: 'utf-8', mode: write.mode });
    // Отдельным вызовом, потому что `mode` у `writeFile` действует только при СОЗДАНИИ файла: у
    // уже существующего права останутся прежними, и без этого разъехавшийся ключ так и остался бы
    // открытым.
    await fs.chmod(write.path, write.mode).catch(() => {});
  }
}
