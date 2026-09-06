import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import type { ServerReadableStream, sendUnaryData } from '@grpc/grpc-js';
import { config } from '../config.js';
import { execAsync, execFileAsync } from '../utils/exec.js';
import { replaceBinaryAtomically } from './binary.service.js';
import { receiveStreamedBinary } from './binaryReceiver.js';

const logger = pino({ level: 'info' });

/**
 * Установка бинаря mihomo — ядра ТЫЛОВОГО инстанса.
 *
 * Тыл — второй прокси на том же узле, за фронтовым sing-box: фронт принимает абонентов, тыл
 * решает, что уходит в WARP, а что выходит с собственного адреса узла. Раньше тылом работал второй
 * инстанс того же sing-box; mihomo пришёл ради `rule-providers` — поддерживаемых списков доменов,
 * которых у sing-box нет.
 *
 * **Capabilities не выдаются, в отличие от sing-box.** Тому нужны `cap_net_admin` (TUN) и
 * `cap_net_bind_service` (порты ниже 1024). Тыл не поднимает интерфейсов и слушает только
 * `127.0.0.1:29000/29001` — права ему не нужны ни одни, а выданные лишними были бы расширением
 * поверхности атаки без единой причины.
 */

/**
 * Проверяет, что НОВЫЙ бинарь принимает конфиг, который на узле уже работает.
 *
 * Смысл тот же, что у `verifyBinaryAcceptsLiveConfigs` для sing-box, и причина та же: конфиг
 * проверяется при каждом применении, но проверяет его ТЕКУЩИЙ бинарь. Смена бинаря меняет судью —
 * сборки различаются набором фич и строгостью разбора. Последовательность «подменили → перезапуск →
 * не стартует» оставляет узел без тыла, и откатываться не на что: `replaceBinaryAtomically` старый
 * бинарь не сохраняет.
 *
 * Отсутствие конфига — не отказ: бинарь может приезжать раньше первой настройки тыла, это штатный
 * порядок (сначала ядро, потом конфигурация).
 */
export async function verifyMihomoAcceptsLiveConfig(
  binaryPath: string
): Promise<{ ok: true } | { ok: false; configPath: string; error: string }> {
  const configPath = config.REAR_MIHOMO_CONFIG_PATH;
  const exists = await fs.stat(configPath).then(() => true).catch(() => false);
  if (!exists) return { ok: true };

  try {
    // `-d` задаёт рабочий каталог: mihomo ищет в нём наборы правил и складывает своё состояние.
    await execFileAsync(binaryPath, ['-t', '-d', path.dirname(configPath), '-f', configPath]);
    return { ok: true };
  } catch (err: unknown) {
    const e = err as { stderr?: string; message?: string };
    return { ok: false, configPath, error: (e.stderr || e.message || 'unknown error').slice(0, 1000) };
  }
}

/**
 * RPC UploadMihomoBinary (клиентский стрим).
 */
export async function uploadMihomoBinaryHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  return receiveStreamedBinary(
    call,
    callback,
    { rpcName: 'UploadMihomoBinary', tempPrefix: 'mihomo' },
    async ({ tempPath, version }) => {
      const targetPath = config.MIHOMO_BINARY_PATH;
      await fs.chmod(tempPath, 0o755);

      if (process.env.NODE_ENV !== 'test') {
        // Дымовой тест: файл вообще запускается на этой машине. Ловит и битую загрузку, и
        // несовпадение микроархитектуры — второе иначе проявилось бы как SIGILL уже под нагрузкой.
        try {
          await execFileAsync(tempPath, ['-v']);
        } catch (err: unknown) {
          const msg = err instanceof Error ? err.message : String(err);
          return { success: false, message: `Полученный бинарь mihomo не запускается: ${msg.slice(0, 300)}` };
        }

        const accepts = await verifyMihomoAcceptsLiveConfig(tempPath);
        if (!accepts.ok) {
          // Подмены НЕ было: работающий тыл остаётся на прежнем бинаре и прежнем конфиге.
          logger.warn(
            { configPath: accepts.configPath, version },
            'Rejected mihomo binary: it does not accept the config already live on this node'
          );
          return {
            success: false,
            message:
              `Бинарь mihomo ${version} отвергнут: он не принимает уже применённый конфиг ` +
              `${accepts.configPath}. Прежний бинарь оставлен на месте. ${accepts.error.slice(0, 400)}`,
          };
        }
      }

      await replaceBinaryAtomically(tempPath, targetPath);
      logger.info({ path: targetPath, version }, 'Atomically updated mihomo binary');

      // Юнит здесь НЕ создаётся: тыл провижинится вместе со своим конфигом (ConfigureRearSingbox с
      // core=mihomo), потому что запускать ядро без конфигурации не во что. Если юнит уже есть —
      // перезапускаем, чтобы новая версия начала работать сразу, а не после следующей настройки.
      if (process.env.NODE_ENV !== 'test') {
        const unitExists = await fs
          .stat(config.REAR_MIHOMO_UNIT_FILE_PATH)
          .then(() => true)
          .catch(() => false);
        if (unitExists) {
          try {
            await execAsync('systemctl restart route-rear-mihomo');
          } catch (err: unknown) {
            const msg = err instanceof Error ? err.message : String(err);
            logger.warn({ err: msg }, 'Failed to restart rear mihomo after binary upgrade');
          }
        }
      }

      return { success: true, message: `mihomo binary version ${version} successfully updated` };
    }
  );
}

/**
 * Допустимое имя набора правил.
 *
 * Имя приходит ПО СЕТИ и становится частью имени файла — это единственное место во всём приёме, где
 * недоверенная строка попадает в путь. Проверка на разрешённый набор символов, а не на запрещённые:
 * чёрные списки обходятся кодировками, белый — нет. Восклицательный знак разрешён потому, что у
 * MetaCubeX так помечены наборы «без китайского сегмента» (`category-ai-!cn`).
 */
const SAFE_RULE_SET_NAME = /^[A-Za-z0-9][A-Za-z0-9._!-]{0,63}$/;

/**
 * RPC UploadRearRuleSet — приём одного набора правил для тыла.
 *
 * Узел за наборами в сеть не ходит: их привозит оркестратор. Здесь только приём и запись.
 */
export async function uploadRearRuleSetHandler(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>
): Promise<void> {
  return receiveStreamedBinary(
    call,
    callback,
    { rpcName: 'UploadRearRuleSet', tempPrefix: 'ruleset' },
    async ({ tempPath, targetBinary, bytes }) => {
      const name = targetBinary;
      if (!SAFE_RULE_SET_NAME.test(name)) {
        logger.warn({ name: name.slice(0, 120) }, 'Rejected rule set with a disallowed name');
        return { success: false, message: 'Недопустимое имя набора правил.' };
      }

      const dir = config.REAR_RULE_SET_DIR;
      await fs.mkdir(dir, { recursive: true });
      const destPath = path.join(dir, `${name}.mrs`);

      // Вторая линия обороны на случай, если шаблон выше однажды ослабят: итоговый путь обязан
      // лежать ровно в каталоге наборов, а не «где-то под ним» и тем более не выше.
      if (path.dirname(path.resolve(destPath)) !== path.resolve(dir)) {
        logger.error({ name, destPath }, 'Rule set path escaped its directory — refusing to write');
        return { success: false, message: 'Недопустимое имя набора правил.' };
      }

      // Запись атомарная: mihomo может читать наборы в этот самый момент, и наполовину
      // переписанный файл он бы отверг целиком, оставив тыл без правил.
      const stagingPath = path.join(dir, `.${name}.mrs.${Date.now()}.tmp`);
      try {
        await fs.copyFile(tempPath, stagingPath);
        await fs.chmod(stagingPath, 0o644);
        await fs.rename(stagingPath, destPath);
      } finally {
        await fs.unlink(stagingPath).catch(() => {});
      }

      logger.info({ name, bytes, destPath }, 'Rear rule set stored');

      /**
       * Перечитать конфиг ПОСЛЕ записи — иначе новый список лежит на диске мёртвым грузом: провайдеры
       * читаются при разборе конфига, а не при обращении к правилу.
       *
       * Только если тыл УЖЕ работает. При переключении ядра наборы приезжают раньше конфига, юнита
       * ещё нет, и перезагружать нечего — а конфиг, пришедший следом, прочитает файлы сам.
       *
       * `reload`, а не `restart`: у юнита это SIGHUP, mihomo перечитывает конфиг не завершаясь
       * (проверено на живом бинаре 2026-09-06). Через тыл идёт весь звёздный трафик узла, и рвать
       * его ради обновления списка доменов было бы несоразмерно.
       */
      if (process.env.NODE_ENV !== 'test') {
        const unit = path.basename(config.REAR_MIHOMO_UNIT_FILE_PATH, '.service');
        const active = await execAsync(`systemctl is-active ${unit}`)
          .then(({ stdout }) => stdout.trim() === 'active')
          .catch(() => false);
        if (active) {
          await execAsync(`systemctl reload ${unit}`).catch((err: unknown) => {
            const msg = err instanceof Error ? err.message : String(err);
            logger.warn({ err: msg, name }, 'Failed to reload rear mihomo after storing a rule set');
          });
        }
      }

      return { success: true, message: `Набор правил ${name} записан (${bytes} байт)` };
    }
  );
}
