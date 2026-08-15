import path from 'path';
import * as fs from 'fs/promises';
import pino from 'pino';
import { execAsync } from './exec.js';
import { config } from '../config.js';

const logger = pino({ level: 'info' });

const CADDY_SYSTEMD_OVERRIDE_PATH = '/etc/systemd/system/caddy.service.d/override.conf';
const CADDY_CLOUDFLARE_ENV_PATH = '/etc/caddy/cloudflare.env';

/**
 * Провижинит systemd drop-in override, переключающий apt-owned юнит `caddy.service` на кастомно
 * собранный бинарник (caddy-dns/cloudflare, нужен для DNS-01 wildcard-сертификата). Никогда не
 * трогает сам unit-файл apt-пакета — только добавляет override рядом (пустой `ExecStart=`
 * обязателен, сбрасывает исходный ExecStart перед переопределением, иначе systemd просто
 * добавит вторую директиву поверх первой). `EnvironmentFile=-...` (с минусом) — файл с
 * Cloudflare API-токеном может ещё не существовать на момент провижининга override (доставляется
 * отдельным шагом через ConfigureCaddy), минус говорит systemd не падать, если файла пока нет.
 *
 * Вызывается только явно при сидировании Xeon-ring ноды (XeonService.seedNewXeonNode) — обычные
 * egress-ноды этот путь никогда не затрагивают, agent сам не знает о роли ноды.
 */
export async function ensureCaddyCustomBinaryOverride(
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<boolean> {
  const binaryPath = config.CADDY_BINARY_PATH || '/usr/local/bin/caddy-custom';

  const expectedContent = `[Service]
ExecStart=
ExecStart=${binaryPath} run --environ --config ${config.CADDYFILE_PATH || '/etc/caddy/Caddyfile'}
EnvironmentFile=-${CADDY_CLOUDFLARE_ENV_PATH}
`;

  const existingContent = await fs.readFile(CADDY_SYSTEMD_OVERRIDE_PATH, 'utf-8').catch(() => null);
  if (existingContent === expectedContent) {
    logger.debug({ path: CADDY_SYSTEMD_OVERRIDE_PATH }, 'Caddy systemd override is already up to date; skipping write and daemon-reload');
    return false;
  }

  await fs.mkdir(path.dirname(CADDY_SYSTEMD_OVERRIDE_PATH), { recursive: true });
  await fs.writeFile(CADDY_SYSTEMD_OVERRIDE_PATH, expectedContent, 'utf-8');
  logger.info({ path: CADDY_SYSTEMD_OVERRIDE_PATH, binaryPath }, 'Provisioned/updated Caddy systemd override to use custom binary');

  if (process.env.NODE_ENV !== 'test') {
    await runExec('systemctl daemon-reload');
    logger.info('Executed systemctl daemon-reload after updating Caddy systemd override');
  }

  return true;
}

const ALLOWED_CAMOUFLAGE_DIRS = [
  path.resolve('/var/www'),
  path.resolve('/tmp/camouflage'),
  path.resolve('/opt/route-agent/decoy'),
  path.resolve(process.cwd(), 'tests/temp')
];

/**
 * Валидирует и резолвит путь для фасадной заглушки Caddy, предотвращая Path Traversal.
 */
export function validateSafeCamouflagePath(targetPath: string): string {
  if (!targetPath || typeof targetPath !== 'string') {
    throw new Error('Invalid path provided');
  }

  const resolved = path.resolve(targetPath);

  const isAllowed = ALLOWED_CAMOUFLAGE_DIRS.some((allowedDir) => {
    const relative = path.relative(allowedDir, resolved);
    return !relative.startsWith('..') && !path.isAbsolute(relative);
  });

  if (!isAllowed) {
    throw new Error(`Path Traversal restriction: ${targetPath} is not within allowed directories`);
  }

  return resolved;
}

// Захардкожены в соответствии со значениями по умолчанию xhttpSocket/grpcSocket
// в CaddyfileGenerator (route-orchestrator, src/core/generators/caddy.ts) — эти
// unix-сокеты создаёт собственный процесс xray-core Remnawave (не код этого репозитория),
// и ничто в текущей кодовой базе (ни здесь, ни в оркестраторе) их не переопределяет.
// Если оркестратор когда-либо сделает эти пути конфигурируемыми, список ниже нужно
// будет держать в синхронизации с ним вручную.
const XRAY_UNIX_SOCKET_PATHS = [
  '/dev/shm/vless-xhttp.sock',
  '/dev/shm/vless-grpc.sock',
];

/**
 * Гарантирует, что Caddy (пользователь `caddy`) может дозвониться до unix-сокетов
 * VLESS XHTTP/gRPC, которые создаёт независимо управляемый процесс xray-core Remnawave.
 *
 * Caddyfile, который генерирует route-orchestrator, всегда проксирует на эти два
 * захардкоженных сокета (см. CaddyfileGenerator), но xray-core запускается не от
 * пользователя caddy, а от какого-то своего — какого именно, эта нода не знает и не
 * контролирует (Remnawave — отдельно управляемая панель/процесс, а не то, что
 * провижинится этим репозиторием). Поэтому здесь не подходит chown/группа: нет
 * гарантированного общего пользователя/группы, в которую можно было бы добавить caddy.
 * Вместо этого используется `chmod 666` (world-writable) — эти сокеты лежат в /dev/shm
 * (tmpfs) и доступны только локальным процессам на этом же хосте, а не по сети, так что
 * по факту это тот же уровень экспозиции, что и у любого другого локального IPC-сокета
 * в схеме "Caddy перед xray-core". Мягкая деградация: если сокета ещё нет (VLESS не
 * активен на ноде, либо xray-core ещё не поднялся), путь молча пропускается — это не
 * ошибка и не повод для warn (симметрично тому, как fixCaddyPermissions ведёт себя при
 * отсутствии /var/lib/caddy). Ошибка chmod на одном сокете не должна прерывать обработку
 * остальных путей и тем более — RPC ConfigureCaddy или старт агента.
 *
 * `paths`/`runExec` параметризованы только ради юнит-тестов (см. tests/app.test.ts);
 * все боевые вызовы используют значения по умолчанию.
 */
export async function fixXraySocketPermissions(
  paths: string[] = XRAY_UNIX_SOCKET_PATHS,
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<void> {
  for (const socketPath of paths) {
    const socketExists = await fs.stat(socketPath).then(() => true).catch(() => false);
    if (!socketExists) {
      continue;
    }

    try {
      await runExec(`chmod 666 ${socketPath}`);
    } catch (err: any) {
      logger.warn({ err: err.message, socketPath }, 'Failed to adjust permissions on Xray unix socket');
    }
  }
}
