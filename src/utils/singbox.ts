import * as fs from 'fs/promises';
import path from 'path';
import crypto from 'crypto';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync, execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

const CADDY_LIB_DIR = '/var/lib/caddy';

/**
 * Гарантирует права на чтение сертификатов Caddy для ядра sing-box.
 *
 * Официальный apt-пакет caddy на Debian/Ubuntu всегда создаёт системного
 * пользователя/группу `caddy` и раскладывает `/var/lib/caddy` под ним —
 * это стандартное поведение пакета, а не особенность конкретной ноды,
 * поэтому владелец захардкожен, а не определяется через `getent`/`id`
 * (лишний exec и лишняя точка отказа ради значения, которое не варьируется
 * на поддерживаемых дистрибутивах). chmod сам по себе не чинит выданный
 * root:root каталог — процесс Caddy всегда работает от caddy:caddy
 * (systemctl show caddy -p User -p Group), поэтому chown обязателен;
 * chmod оставлен следом как дополнительная гарантия на случай, если
 * владелец уже корректен, но биты доступа слишком строгие.
 * chown -R рекурсивно переустанавливает владельца на всех уже существующих
 * файлах/подкаталогах при каждом вызове, поэтому одного этого фикса
 * достаточно, чтобы починить ранее испорченные ноды — без отдельной миграции.
 *
 * `dir`/`runExec` параметризованы только ради юнит-тестов (см. tests/app.test.ts);
 * все боевые вызовы используют значения по умолчанию.
 */
export async function fixCaddyPermissions(
  dir: string = CADDY_LIB_DIR,
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<void> {
  try {
    const caddyDirExists = await fs.stat(dir).then(() => true).catch(() => false);
    if (caddyDirExists) {
      await runExec(`chown -R caddy:caddy ${dir} || true`);
      await runExec(`chmod -R 755 ${dir} || true`);
    }
  } catch (err: any) {
    logger.warn({ err: err.message }, 'Failed to adjust Caddy certificates permissions');
  }
}

/**
 * Идемпотентно создаёт или обновляет systemd unit-файл sing-box.
 *
 * Раньше unit статично прописывался в install.sh на этапе провижининга ноды — единственное
 * из трёх доп. ядер (sing-box/AWG/Olcrtc), нарушавшее принятый в проекте паттерн: AWG
 * (ensureAwgSystemdUnit, src/services/systemdUnit.service.ts) и Olcrtc (configureOlcrtcHandler,
 * src/services/config.service.ts) оба создают свои unit-файлы лениво, в момент первой реальной
 * команды от оркестратора, а не заранее в install.sh. Эта функция зеркалит
 * ensureAwgSystemdUnit: пишет файл только если он отсутствует или его содержимое отличается
 * от ожидаемого, daemon-reload выполняется только при реальном изменении.
 *
 * `overridePath`/`runExec` параметризованы только ради юнит-тестов; боевые вызовы
 * (uploadSingboxBinaryHandler) используют значения по умолчанию.
 */
export async function ensureSingboxSystemdUnit(
  overridePath?: string,
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<boolean> {
  const unitPath = overridePath || config.SINGBOX_UNIT_FILE_PATH || '/etc/systemd/system/sing-box.service';
  const binaryPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';
  const configPath = config.SINGBOX_CONFIG_PATH || '/etc/sing-box/config.json';

  /**
   * Содержимое перенесено дословно из install.sh — с ОДНОЙ правкой, стоившей отдельного разбора.
   *
   * ⚠️ ЗДЕСЬ БЫЛА ПАРА `CapabilityBoundingSet`/`AmbientCapabilities` НА CAP_NET_ADMIN И
   * CAP_NET_BIND_SERVICE, И ОНА ЛОМАЛА ПЕРЕЗАГРУЗКУ. Обе строки имеют смысл только когда служба
   * работает под непривилегированным пользователем: ambient-возможности нужны, чтобы выдать их
   * такому процессу. У нас `User=` не задан вовсе, процесс идёт от root — и ему эти возможности
   * безразличны, у него и так все.
   *
   * Зато ограничивающий набор действует и на root, и на ВСЕ процессы юнита, включая `ExecReload`.
   * Он оставлял им ровно две перечисленные возможности и отбирал остальные — в том числе
   * `CAP_KILL`. Пока и служба, и перезагрузка идут от root по одному UID, сигнал проходит и без
   * неё. Но если процесс принадлежит ДРУГОМУ пользователю, `kill` без `CAP_KILL` получает
   * `Operation not permitted` — и перезагрузка не работает никогда.
   *
   * Случай не выдуманный (нода mo-nl-node, 2026-09-04): там раньше стоял пакетный sing-box,
   * работавший под пользователем `sing-box`; наш юнит лёг поверх пакетного, и с этого момента
   * каждый `systemctl reload` падал. Процесс три недели держал в памяти старый конфиг, на UDP 443
   * не слушал никто, а конфиг на диске откатывался в заглушку. Снаружи это выглядело как таймауты
   * hysteria2 и tuic при живом VLESS — потому что VLESS терминирует Caddy, а он перезагружается
   * отдельно и успешно.
   *
   * Если когда-нибудь появится `User=`, обе строки придётся вернуть — вместе с `CAP_KILL`.
   */
  const expectedContent = `[Unit]
Description=sing-box service
After=network.target nss-lookup.target

[Service]
ExecStart=${binaryPath} run -c ${configPath}
Restart=always
RestartSec=5
ExecReload=/bin/sh -c "${binaryPath} check -c ${configPath} && /bin/kill -HUP $MAINPID"

[Install]
WantedBy=multi-user.target
`;

  try {
    const existingContent = await fs.readFile(unitPath, 'utf-8').catch(() => null);

    if (existingContent === expectedContent) {
      logger.debug({ path: unitPath }, 'sing-box systemd unit file is already up to date; skipping write and daemon-reload');
      return false;
    }

    await fs.mkdir(path.dirname(unitPath), { recursive: true });
    await fs.writeFile(unitPath, expectedContent, 'utf-8');
    logger.info({ path: unitPath }, 'Provisioned/updated sing-box systemd unit file');

    if (process.env.NODE_ENV !== 'test') {
      try {
        await runExec('systemctl daemon-reload');
        logger.info('Executed systemctl daemon-reload after updating sing-box systemd unit file');
      } catch (err: any) {
        logger.warn({ err: err.message }, 'Failed to execute systemctl daemon-reload');
      }
    }

    return true;
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.error({ err: msg, path: unitPath }, 'Failed to ensure sing-box systemd unit file');
    return false;
  }
}

/**
 * Defense-in-depth (не основной фикс, а дешёвая страховка): перед мягким reload'ом sing-box
 * проверяет реальное состояние юнита. Юнит может существовать, но быть неактивным по причине,
 * не связанной с provisioning'ом (краш процесса, исчерпанный лимит Restart=always и т.п.) —
 * `systemctl reload` в этом случае ничего не поднимет, нужен `start`.
 *
 * `runExec` параметризован только ради юнит-теста; продовый вызов (из atomicApplyAndReload)
 * использует значение по умолчанию.
 */
export async function resolveSingboxReloadCommand(
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<string> {
  try {
    const { stdout } = await runExec('systemctl is-active sing-box');
    if (stdout.trim() === 'active') {
      return config.RELOAD_COMMAND;
    }
  } catch {
    // `systemctl is-active` завершается ненулевым кодом (и тем самым реджектит промис
    // из promisify(exec)) для любого состояния, кроме "active" — inactive/failed/unknown
    // юнит. Любой такой исход трактуем как "нужен start, а не reload".
  }
  return 'systemctl start sing-box';
}

/**
 * Вспомогательный метод локальной валидации синтаксиса sing-box перед его применением
 */
export async function validateSingBoxConfig(configObj: object): Promise<{ valid: boolean; error?: string }> {
  if (process.env.NODE_ENV === 'test') {
    return { valid: true };
  }
  await fixCaddyPermissions();
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const checkFilePath = path.join(targetDir, `.config.check_${uniqueId}.json`);
  const binaryPath = config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box';

  try {
    await fs.mkdir(targetDir, { recursive: true });
    await fs.writeFile(checkFilePath, JSON.stringify(configObj, null, 2), 'utf-8');

    // Выполняем нативный тест синтаксиса sing-box
    await execFileAsync(binaryPath, ['check', '-c', checkFilePath]);
    return { valid: true };
  } catch (err: any) {
    logger.error({ stderr: err.stderr }, 'Sing-box configuration syntax check failed');
    return { valid: false, error: err.stderr || err.message };
  } finally {
    await fs.unlink(checkFilePath).catch(() => {});
  }
}

/**
 * Исполнитель применения конфигурации и мягкой перезагрузки ядра
 */
export async function atomicApplyAndReload(configObj: object): Promise<void> {
  const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempFilePath = path.join(targetDir, `.config.${uniqueId}.tmp`);
  const backupFilePath = `${config.SINGBOX_CONFIG_PATH}.bak`;

  // 1. Сохраняем бэкап текущей конфигурации при её наличии
  const configExists = await fs.stat(config.SINGBOX_CONFIG_PATH).then(() => true).catch(() => false);
  if (configExists) {
    await fs.copyFile(config.SINGBOX_CONFIG_PATH, backupFilePath).catch(() => {});
  }

  // 2. Атомарная подмена через временный файл
  await fs.writeFile(tempFilePath, JSON.stringify(configObj, null, 2), 'utf-8');
  await fs.rename(tempFilePath, config.SINGBOX_CONFIG_PATH);

  // 3. Мягкий reload сервиса с откатом при ошибке
  if (process.env.NODE_ENV !== 'test' || process.env.RELOAD_COMMAND) {
    try {
      // В боевом режиме подстраховываемся: юнит мог существовать, но быть неактивным
      // по причине, не связанной с этим конкретным изменением конфига (см.
      // resolveSingboxReloadCommand). В тестах поведение не меняем — используем
      // config.RELOAD_COMMAND напрямую, как и раньше.
      const reloadCmd = process.env.NODE_ENV !== 'test'
        ? await resolveSingboxReloadCommand()
        : config.RELOAD_COMMAND;
      const { stdout, stderr } = await execAsync(reloadCmd);
      if (stdout) logger.info({ stdout }, 'Reload command stdout');
      if (stderr) logger.warn({ stderr }, 'Reload command stderr');
    } catch (err) {
      if (configExists) {
        await fs.copyFile(backupFilePath, config.SINGBOX_CONFIG_PATH).catch(() => {});
      }
      throw err;
    }
  }
}
