import * as fs from 'fs/promises';
import path from 'path';
import crypto from 'crypto';
import { config } from '../config.js';
import { execFileAsync } from './exec.js';
import { validateSingBoxConfig } from './singbox.js';

/**
 * Тыловой инстанс умеет работать на двух ядрах, и это описание их различий.
 *
 * **ЗАЧЕМ ДВА.** Тыл заводился на втором инстансе sing-box — том же бинаре, что обслуживает
 * абонентов на фронте. Потом выяснилось, что в WARP уходит весь трафик звёздной ветки без разбора
 * по доменам, и весь обычный веб узла выходит с единственного адреса Cloudflare, общего со всеми
 * клиентами WARP региона. Разделять нужно по спискам доменов, а у mihomo для этого есть
 * `rule-providers`, которых у sing-box нет.
 *
 * **ЗАЧЕМ ОПИСАНИЕМ, А НЕ РАЗВИЛКАМИ.** Ядра различаются в шести местах: путь бинаря, путь конфига,
 * путь юнита, содержимое юнита, способ проверки конфига и рабочий каталог. Разложи это по `if` в
 * обработчике — и каждая новая операция над тылом обязана вспомнить про все шесть, а забытая ветка
 * не сломается заметно: она просто сделает что-то не тому ядру.
 *
 * **ЧЕГО ЗДЕСЬ НЕТ.** Ничего про systemd, кроме текста юнита: запуск, остановка и чтение состояния
 * живут в вызывающем сервисе. Разница между ядрами в этих операциях исчерпывается именем юнита, а
 * оно уже здесь.
 */

export type RearCoreId = 'singbox' | 'mihomo';

export interface RearCore {
  id: RearCoreId;
  /** Человекочитаемое имя для сообщений админу. */
  label: string;
  binaryPath(): string;
  configPath(): string;
  unitPath(): string;
  unitName(): string;
  /** Полное содержимое systemd-юнита, включая ExecStart и ExecReload. */
  unitContent(): string;
  /** Проверка конфига ТЕМ САМЫМ бинарём, который его будет исполнять. */
  validate(configObj: object): Promise<{ valid: boolean; error?: string }>;
}

const unitName = (unitPath: string) => path.basename(unitPath, '.service');

/**
 * Общий каркас юнита. Различаются только описание и две команды — всё остальное одинаково, и
 * держать два почти одинаковых текста означало бы однажды поправить `Restart=` в одном из них.
 */
function buildUnit(params: { description: string; execStart: string; execReload: string }): string {
  return `[Unit]
Description=${params.description}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${params.execStart}
Restart=on-failure
RestartSec=5
ExecReload=${params.execReload}

[Install]
WantedBy=multi-user.target
`;
}

export const SINGBOX_REAR_CORE: RearCore = {
  id: 'singbox',
  label: 'sing-box',
  binaryPath: () => config.SINGBOX_BINARY_PATH || '/usr/local/bin/sing-box',
  configPath: () => config.REAR_SINGBOX_CONFIG_PATH || '/etc/route-agent/rear.json',
  unitPath: () => config.REAR_SINGBOX_UNIT_FILE_PATH || '/etc/systemd/system/route-rear-singbox.service',
  unitName() {
    return unitName(this.unitPath());
  },
  unitContent() {
    const binary = this.binaryPath();
    const cfg = this.configPath();
    return buildUnit({
      description: 'Rear sing-box (WARP egress, managed by route-agent)',
      execStart: `${binary} run -c ${cfg}`,
      execReload: `/bin/sh -c "${binary} check -c ${cfg} && /bin/kill -HUP $MAINPID"`,
    });
  },
  validate: validateSingBoxConfig,
};

export const MIHOMO_REAR_CORE: RearCore = {
  id: 'mihomo',
  label: 'mihomo',
  binaryPath: () => config.MIHOMO_BINARY_PATH || '/usr/local/bin/mihomo',
  configPath: () => config.REAR_MIHOMO_CONFIG_PATH || '/etc/route-agent/rear.yaml',
  unitPath: () => config.REAR_MIHOMO_UNIT_FILE_PATH || '/etc/systemd/system/route-rear-mihomo.service',
  unitName() {
    return unitName(this.unitPath());
  },
  unitContent() {
    const binary = this.binaryPath();
    const cfg = this.configPath();
    const workDir = path.dirname(cfg);
    // `-d` — рабочий каталог: mihomo ищет в нём наборы правил и складывает своё состояние.
    //
    // ExecReload через SIGHUP, а не restart: проверено на живом бинаре 2026-09-06 — mihomo
    // перечитывает конфиг по этому сигналу, не завершаясь (`mode` сменился на лету, pid остался
    // прежним). Разница не косметическая: через тыл идёт весь звёздный трафик узла, и перезапуск
    // оборвал бы его целиком ради правки, которая обычно сводится к одному ключу WARP.
    return buildUnit({
      description: 'Rear mihomo (WARP egress with rule sets, managed by route-agent)',
      execStart: `${binary} -d ${workDir} -f ${cfg}`,
      execReload: `/bin/sh -c "${binary} -t -d ${workDir} -f ${cfg} && /bin/kill -HUP $MAINPID"`,
    });
  },
  validate: validateMihomoConfig,
};

/**
 * Проверяет конфиг тыла тем бинарём mihomo, который его и будет исполнять.
 *
 * Проверка идёт по ВРЕМЕННОМУ файлу рядом с боевым, а не поверх него: отвергнутый конфиг не должен
 * даже на мгновение оказаться тем, что прочитает перезапустившийся процесс.
 */
async function validateMihomoConfig(configObj: object): Promise<{ valid: boolean; error?: string }> {
  if (process.env.NODE_ENV === 'test') {
    return { valid: true };
  }

  const targetPath = MIHOMO_REAR_CORE.configPath();
  const workDir = path.dirname(targetPath);
  const checkPath = path.join(workDir, `.rear.check_${Date.now()}_${crypto.randomUUID().slice(0, 8)}.yaml`);

  try {
    await fs.mkdir(workDir, { recursive: true });
    await fs.writeFile(checkPath, JSON.stringify(configObj, null, 2), 'utf-8');
    await execFileAsync(MIHOMO_REAR_CORE.binaryPath(), ['-t', '-d', workDir, '-f', checkPath]);
    return { valid: true };
  } catch (err: unknown) {
    const e = err as { stderr?: string; message?: string };
    return { valid: false, error: (e.stderr || e.message || 'unknown error').slice(0, 1000) };
  } finally {
    await fs.unlink(checkPath).catch(() => {});
  }
}

/**
 * Ядро по значению из запроса. Пустая строка — sing-box: так читается умолчание proto3 у агента,
 * которому оркестратор поля `core` ещё не присылает.
 */
export function resolveRearCore(coreId: string | undefined | null): RearCore {
  return coreId === 'mihomo' ? MIHOMO_REAR_CORE : SINGBOX_REAR_CORE;
}

/** Второе ядро — то, которое надо снять при переключении. */
export function otherRearCore(core: RearCore): RearCore {
  return core.id === 'mihomo' ? SINGBOX_REAR_CORE : MIHOMO_REAR_CORE;
}
