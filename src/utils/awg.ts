import path from 'path';
import pino from 'pino';
import { config } from '../config.js';
import { execAsync } from './exec.js';

const logger = pino({ level: 'info' });

/**
 * Извлекает имя основного AWG-интерфейса из конфигурационного пути (AWG_CONFIG_PATH).
 * Единственный источник истины для имени AWG-интерфейса в агенте.
 */
export function getAwgInterfaceName(): string {
  const configPath = config.AWG_CONFIG_PATH || '/etc/amnezia/amneziawg/awg0.conf';
  const name = path.basename(configPath, '.conf');
  return name || 'awg0';
}

export interface RestartAwgResult {
  reloaded: boolean;
  restartedUnits: string[];
}

/**
 * Перезапускает все активные инстансы AWG-сервисов (route-awg@*.service / awg-quick@*.service).
 * В multi-instance сценарии перезапускает все запущенные интерфейсы.
 * Если ни один интерфейс не запущен или ещё не сконфигурирован, логирует info и пропускает рестарт.
 */
export async function restartActiveAwgServices(): Promise<RestartAwgResult> {
  const primaryIface = getAwgInterfaceName();
  const activeUnits = new Set<string>();

  try {
    const { stdout } = await execAsync("systemctl list-units 'route-awg@*' 'awg-quick@*' --state=active --plain --no-legend");
    const lines = stdout.trim().split('\n');
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const match = trimmed.match(/^(route-awg@[^\s]+|awg-quick@[^\s]+)/);
      if (match) {
        activeUnits.add(match[1]);
      }
    }
  } catch {
    // Игнорируем ошибку выполнения list-units
  }

  // Если список активных юнитов пуст, дополнительно проверяем статус основного юнита route-awg@<primaryIface>
  if (activeUnits.size === 0) {
    try {
      const primaryUnit = `route-awg@${primaryIface}`;
      const { stdout } = await execAsync(`systemctl is-active ${primaryUnit}`);
      if (stdout.trim() === 'active') {
        activeUnits.add(primaryUnit);
      }
    } catch {}
  }

  if (activeUnits.size === 0) {
    logger.info(
      { iface: primaryIface },
      `AWG interface ${primaryIface} not yet configured, skipping restart — will use new binary on first configure`
    );
    return { reloaded: false, restartedUnits: [] };
  }

  const restartedUnits: string[] = [];
  for (const unit of activeUnits) {
    try {
      await execAsync(`systemctl restart ${unit}`);
      logger.info({ unit }, `Successfully restarted active AWG service ${unit}`);
      restartedUnits.push(unit);
    } catch (err: any) {
      logger.warn({ unit, err: err.message }, `Failed to restart AWG service ${unit}`);
    }
  }

  return {
    reloaded: restartedUnits.length > 0,
    restartedUnits
  };
}
