import * as fs from 'fs/promises';
import path from 'path';
import pino from 'pino';
import { config } from '../config.js';
import { execFileAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Настройка сетевых буферов ядра под QUIC.
 *
 * ЗАЧЕМ ВООБЩЕ. Внутри sing-box (hysteria2 и tuic) работает quic-go, и при подъёме сокета он просит
 * буфер приёма на 7,5 МБ. Умолчание `net.core.rmem_max` — 212992 байта, и ядро молча срезает запрос
 * в тридцать пять раз. Проверено на ядре 6.6: при умолчании сокет, запросивший 7500000, получает
 * ровно 212992; после поднятия потолка — все 7500000. Единственный след в логах — строка quic-go
 * `failed to sufficiently increase receive buffer size`. То есть нода тихо работает с окном приёма
 * в 208 КБ, и упирается не в канал, а в ядро.
 *
 * ПОЧЕМУ ЧИСЛА МОЖНО ЗАШИТЬ. `rmem_max`/`wmem_max` — константа ядра, не производная от объёма
 * памяти: на машине с 7,3 ГБ значение то же самое, что документировано для любой другой. Поэтому
 * подгонять их под конкретную ноду нечего. Соседние `udp_mem`, `tcp_rmem`, `tcp_wmem` наоборот
 * считаются от RAM при загрузке — их здесь намеренно НЕТ, трогать их вслепую значило бы заменить
 * разумное умолчание своей выдумкой.
 *
 * ЛОВУШКА, НА КОТОРОЙ ЛЕГКО ОШИБИТЬСЯ ВДВОЕ: ядро хранит удвоенное значение и `getsockopt` вернёт
 * 15000000 при запрошенных 7,5 МБ. Но потолок сравнивается с ЗАПРОШЕННЫМ числом, а не с удвоенным,
 * поэтому здесь стоит 7500000 без всякого умножения.
 *
 * ЧЕГО ЗДЕСЬ НЕТ И ПОЧЕМУ. `rmem_default` не трогаем: он раздаёт буфер каждому сокету, включая
 * тысячи короткоживущих, то есть покупает расход памяти без выигрыша — нам нужен потолок для того,
 * кто просит явно. BBR и `default_qdisc=fq` отложены сознательно: доступность BBR зависит от сборки
 * ядра (в ядре WSL, где числа проверялись, его нет вовсе — только `reno cubic`), и включать его
 * не проверив `tcp_available_congestion_control` на самой ноде нельзя.
 */
export interface KernelTuningEntry {
  key: string;
  value: string;
  /** Зачем эта строка нужна — идёт комментарием в файл, чтобы на ноде не гадали. */
  why: string;
}

export const KERNEL_TUNING: readonly KernelTuningEntry[] = [
  {
    key: 'net.core.rmem_max',
    value: '7500000',
    why: 'потолок буфера приёма: quic-go (hysteria2/tuic) просит 7.5 МБ, умолчание 212992 режет его в 35 раз',
  },
  {
    key: 'net.core.wmem_max',
    value: '7500000',
    why: 'то же для отправки',
  },
  {
    key: 'net.core.netdev_max_backlog',
    value: '8192',
    why: 'очередь приёма при всплесках pps; умолчание 1000 родом из эпохи 100 Мбит',
  },
  {
    key: 'net.ipv4.ip_local_port_range',
    value: '10240 65535',
    why: 'исходящие порты: умолчание 32768-60999 даёт ~28k на адрес, а egress-нода ходит наружу с одного IP за всех',
  },
];

export interface KernelTuningKeyResult {
  key: string;
  applied: boolean;
  /** Значение, которое ядро отдало ПОСЛЕ применения. `null` — прочитать не удалось. */
  effective: string | null;
  error?: string;
}

export interface KernelTuningResult {
  /** Переписан ли файл в /etc/sysctl.d. `false` — уже лежал ровно такой. */
  fileChanged: boolean;
  keys: KernelTuningKeyResult[];
}

/**
 * Файл для `/etc/sysctl.d`. Нужен не вместо применения, а вместе с ним: `sysctl -w` живёт до
 * перезагрузки, файл переживает её даже если агент в этот момент не поднялся.
 */
export function renderSysctlFile(entries: readonly KernelTuningEntry[] = KERNEL_TUNING): string {
  const body = entries.map((e) => `# ${e.why}\n${e.key} = ${e.value}`).join('\n\n');
  return `# Управляется route-agent, правки будут перезаписаны при следующем старте агента.\n\n${body}\n`;
}

/**
 * Применяет профиль и возвращает, что из этого вышло.
 *
 * ПО КЛЮЧАМ, А НЕ `sysctl -p` ЦЕЛИКОМ: на OpenVZ и в контейнерах часть ключей доступна только для
 * чтения, и один такой не должен уносить с собой остальные. По той же причине неудача — это
 * предупреждение, а не отказ: агент, который не стартует из-за недоступного sysctl, хуже агента с
 * неоптимальными буферами.
 *
 * Значение читается ОБРАТНО после записи, а не берётся из намерения: ядро может принять команду и
 * округлить величину, и знать надо то, что там оказалось на самом деле.
 */
export async function applyKernelTuning(
  runExecFile: typeof execFileAsync = execFileAsync,
  entries: readonly KernelTuningEntry[] = KERNEL_TUNING
): Promise<KernelTuningResult> {
  const fileChanged = await writeSysctlFile(entries);
  const keys: KernelTuningKeyResult[] = [];

  for (const entry of entries) {
    try {
      await runExecFile('sysctl', ['-w', `${entry.key}=${entry.value}`]);
    } catch (err: any) {
      const message = err?.message ? String(err.message) : 'unknown error';
      logger.warn({ key: entry.key, err: message }, 'Kernel tuning: sysctl key could not be set');
      keys.push({ key: entry.key, applied: false, effective: null, error: message });
      continue;
    }

    let effective: string | null = null;
    try {
      const { stdout } = await runExecFile('sysctl', ['-n', entry.key]);
      effective = String(stdout).trim().replace(/\s+/g, ' ');
    } catch {
      // Записалось, а прочитать не смогли — не повод считать применение неудачным.
    }
    keys.push({ key: entry.key, applied: true, effective });
  }

  const failed = keys.filter((k) => !k.applied).map((k) => k.key);
  logger.info(
    { fileChanged, applied: keys.length - failed.length, failed },
    'Kernel tuning: network buffer profile applied'
  );

  return { fileChanged, keys };
}

/** Пишем только при отличии — тем же приёмом, что и systemd-юниты рядом. */
async function writeSysctlFile(entries: readonly KernelTuningEntry[]): Promise<boolean> {
  const target = config.SYSCTL_PROFILE_PATH;
  const expected = renderSysctlFile(entries);

  const existing = await fs.readFile(target, 'utf-8').catch(() => null);
  if (existing === expected) return false;

  await fs.mkdir(path.dirname(target), { recursive: true });
  await fs.writeFile(target, expected, 'utf-8');
  logger.info({ path: target }, 'Kernel tuning: sysctl profile written');
  return true;
}
