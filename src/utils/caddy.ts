import path from 'path';
import * as fs from 'fs/promises';
import pino from 'pino';
import { execAsync } from './exec.js';
import { config } from '../config.js';

const logger = pino({ level: 'info' });

const CADDY_SYSTEMD_OVERRIDE_PATH = '/etc/systemd/system/caddy.service.d/override.conf';
const CADDY_CLOUDFLARE_ENV_PATH = '/etc/caddy/cloudflare.env';
const DEFAULT_CADDY_CUSTOM_BINARY_PATH = '/usr/local/bin/caddy-custom';

/**
 * Resolves which `caddy` binary to invoke for `caddy validate`/CLI calls — checked on disk at
 * CALL TIME, not read from an env var. CADDY_BINARY_PATH is set (if at all) in route-agent's own
 * .env, loaded once at process start via dotenv; uploadCaddyBinaryHandler writing the custom
 * binary to disk mid-lifetime has no way to make an already-running agent process see a new env
 * var without restarting the AGENT itself (a much riskier operation than just restarting Caddy,
 * since it'd have to survive tearing down its own gRPC response mid-flight). Checking existence
 * on disk sidesteps that entirely — no agent restart needed, works immediately after
 * uploadCaddyBinaryHandler's atomic write. Falls back to bare `caddy` (apt, on $PATH) when the
 * custom binary isn't present, matching plain egress nodes' existing, unchanged behavior.
 */
export async function resolveCaddyBinary(): Promise<string> {
  const customPath = config.CADDY_BINARY_PATH || DEFAULT_CADDY_CUSTOM_BINARY_PATH;
  try {
    await fs.access(customPath);
    return customPath;
  } catch {
    return 'caddy';
  }
}

/**
 * Parses /etc/caddy/cloudflare.env (KEY=value, one per line — written by configureCaddyHandler)
 * into a plain env object. `EnvironmentFile=` in the systemd override only applies to processes
 * systemd itself starts (caddy.service's own ExecStart/ExecReload) — an ad-hoc `caddy validate`
 * spawned directly from route-agent via execAsync is NOT one of those, so it never sees
 * CF_API_TOKEN unless explicitly merged into that child process's own env (see
 * configureCaddyHandler's validate call). Empty file/missing file both just yield `{}` — the
 * `{env.CF_API_TOKEN}` placeholder in the Caddyfile then fails validation with a clear "API
 * token appears invalid" error rather than silently doing something else.
 */
export async function loadCaddyDnsProviderEnv(envPath: string = CADDY_CLOUDFLARE_ENV_PATH): Promise<Record<string, string>> {
  const content = await fs.readFile(envPath, 'utf-8').catch(() => '');
  const env: Record<string, string> = {};
  for (const line of content.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;
    const eq = trimmed.indexOf('=');
    if (eq === -1) continue;
    env[trimmed.slice(0, eq)] = trimmed.slice(eq + 1);
  }
  return env;
}

/**
 * Провижинит systemd drop-in override, переключающий apt-owned юнит `caddy.service` на кастомно
 * собранный бинарник (caddy-dns/cloudflare, нужен для DNS-01 wildcard-сертификата). Никогда не
 * трогает сам unit-файл apt-пакета — только добавляет override рядом (пустой `ExecStart=`
 * обязателен, сбрасывает исходный ExecStart перед переопределением, иначе systemd просто
 * добавит вторую директиву поверх первой). `EnvironmentFile=-...` (с минусом) — файл с
 * Cloudflare API-токеном может ещё не существовать на момент провижининга override (доставляется
 * отдельным шагом через ConfigureCaddy), минус говорит systemd не падать, если файла пока нет.
 *
 * `daemon-reload` в одиночку НЕ переключает уже запущенный процесс — systemd просто узнаёт
 * о новом ExecStart, но старый процесс (apt-Caddy) продолжает работать как ни в чём не бывало,
 * пока юнит не перезапущен (реальный инцидент — override был применён, `daemon-reload`
 * выполнен, а `Main PID` двухдневной давности так и остался apt-бинарником). `restart`, а не
 * `reload`: для `Type=notify`-юнита `reload` просит ТЕКУЩИЙ процесс перечитать свой конфиг
 * in-place, не переисполняясь под новый ExecStart — только полный restart реально подхватывает
 * override. Безопасно здесь: Xeon-Caddy не обслуживает живой VPN-трафик (это делает sing-box на
 * этом же хосте), только decoy/control-домен — короткий блип при рестарте не задевает прокси.
 *
 * Вызывается только явно при сидировании/апдейте Xeon-ring ноды — обычные egress-ноды этот путь
 * никогда не затрагивают, agent сам не знает о роли ноды.
 */
export async function ensureCaddyCustomBinaryOverride(
  overridePath: string = CADDY_SYSTEMD_OVERRIDE_PATH,
  runExec: (command: string) => Promise<{ stdout: string; stderr: string }> = execAsync,
): Promise<boolean> {
  const binaryPath = config.CADDY_BINARY_PATH || DEFAULT_CADDY_CUSTOM_BINARY_PATH;

  const caddyfilePath = config.CADDYFILE_PATH || '/etc/caddy/Caddyfile';
  // ExecReload= must be reset+overridden too, same as ExecStart= — the apt unit's own
  // ExecReload=/usr/bin/caddy reload ... otherwise stays live and `systemctl reload caddy`
  // (CADDY_RELOAD_COMMAND's first attempt) keeps invoking the stock binary, which can't parse
  // `tls { dns cloudflare ... }` — confirmed live: journalctl showed reload spawning bare
  // `caddy[pid]`, not `caddy-custom[pid]`, failing with the exact "module not registered" error
  // this whole override exists to fix.
  const expectedContent = `[Service]
ExecStart=
ExecStart=${binaryPath} run --environ --config ${caddyfilePath}
ExecReload=
ExecReload=${binaryPath} reload --config ${caddyfilePath} --force
EnvironmentFile=-${CADDY_CLOUDFLARE_ENV_PATH}
`;

  const existingContent = await fs.readFile(overridePath, 'utf-8').catch(() => null);
  if (existingContent === expectedContent) {
    logger.debug({ path: overridePath }, 'Caddy systemd override is already up to date; skipping write and restart');
    return false;
  }

  await fs.mkdir(path.dirname(overridePath), { recursive: true });
  await fs.writeFile(overridePath, expectedContent, 'utf-8');
  logger.info({ path: overridePath, binaryPath }, 'Provisioned/updated Caddy systemd override to use custom binary');

  if (process.env.NODE_ENV !== 'test') {
    await runExec('systemctl daemon-reload');
    await runExec('systemctl restart caddy');
    logger.info('Restarted caddy.service after updating systemd override to actually switch the running binary');
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
