import dotenv from 'dotenv';
import { z } from 'zod';

dotenv.config();

const configSchema = z.object({
  PORT: z.coerce.number().default(8081), // Ожидаемый gRPC порт управления нодой
  HOST: z.string().default('0.0.0.0'),
  EGRESS_CONTROL_SECRET: z.string(),     // Уникальный токен ноды (верифицируется через gRPC Metadata)
  SINGBOX_CONFIG_PATH: z.string().default('/etc/sing-box/config.json'),
  SINGBOX_BINARY_PATH: z.string().default('/usr/local/bin/sing-box'),
  SINGBOX_UNIT_FILE_PATH: z.string().default('/etc/systemd/system/sing-box.service'),
  RELOAD_COMMAND: z.string().default('systemctl reload sing-box'),
  // Тыловой инстанс sing-box (WARP-выход + наблюдение за направлениями). Отдельный конфиг и
  // отдельный юнит, но ТОТ ЖЕ бинарь, что у фронта: так UpgradeSingbox и SelfUpdate покрывают
  // оба инстанса и не появляется версии, о которой никто не знает.
  REAR_SINGBOX_CONFIG_PATH: z.string().default('/etc/sing-box/rear.json'),
  REAR_SINGBOX_UNIT_FILE_PATH: z.string().default('/etc/systemd/system/route-rear-singbox.service'),
  // Как часто сторож проверяет, есть ли у тыла живые WARP-ключи, и переключает селектор на
  // direct при их отсутствии. Часто и дёшево: один запрос по loopback к соседнему процессу.
  // Смысл именно в скорости — оркестратор узнает ту же новость лишь через час.
  REAR_WARP_GUARD_INTERVAL_MS: z.coerce.number().default(30_000),
  CADDYFILE_PATH: z.string().default('/etc/caddy/Caddyfile'),
  // The `|| systemctl restart caddy` fallback is load-bearing, not decorative: `reload` re-POSTs
  // the Caddyfile to the ALREADY-RUNNING process via its admin API — it never re-reads
  // EnvironmentFile= (see the Xeon-ring cloudflare.env note in caddy.ts), so a process that was
  // first started before that file existed/had content stays permanently blind to CF_API_TOKEN no
  // matter how many times it's reloaded. Only a full restart re-execs and re-reads the env fresh.
  // This must be the schema default itself (not just a fallback string in config.service.ts) —
  // z.string().default() only fires when the env var is entirely ABSENT, so whatever string lands
  // here is unconditionally truthy and any `config.CADDY_RELOAD_COMMAND || '...with restart...'`
  // fallback elsewhere is unreachable dead code.
  CADDY_RELOAD_COMMAND: z.string().default('systemctl reload caddy || systemctl restart caddy'),
  // No default on purpose: unset on regular egress nodes (apt Caddy on $PATH stays authoritative
  // for `caddy validate`/reload there), only set on Xeon-ring nodes after the custom
  // caddy-dns/cloudflare-plugin binary is uploaded — see configureCaddyHandler's `|| 'caddy'` fallback.
  CADDY_BINARY_PATH: z.string().optional(),
  // olcrtc-agent-srv (план `olcrtc-redesign.md`, "Новая архитектура: собственный слой") — replaces
  // the old OLCRTC_BINARY_PATH/OLCRTC_MANAGER_BINARY_PATH (third-party Olcrtc_manager admin
  // daemon, no longer used at all: no HTTP admin surface, no Basic Auth, control is entirely via
  // gRPC + local YAML/systemd, same shape as ConfigureMeshTunnel/AWG). One systemd unit INSTANCE
  // per (user, node) — olcrtc-agent-srv@<user_short_uuid>.service, templated like AWG_UNIT_FILE_PATH.
  OLCRTC_AGENT_SRV_BINARY_PATH: z.string().default('/usr/local/bin/olcrtc-agent-srv'),
  OLCRTC_AGENT_SRV_UNIT_FILE_PATH: z.string().default('/etc/systemd/system/olcrtc-agent-srv@.service'),
  OLCRTC_AGENT_SRV_CONFIG_DIR: z.string().default('/etc/olcrtc-agent-srv'),
  AWG_CONFIG_PATH: z.string().default('/etc/amnezia/amneziawg/awg0.conf'),
  AWG_RELOAD_COMMAND: z.string().default('systemctl restart route-awg@awg0'),
  AWG_TOOLS_BINARY_PATH: z.string().default('/usr/local/bin/awg'),
  AWG_QUICK_BINARY_PATH: z.string().default('/usr/local/bin/awg-quick'),
  AWG_GO_BINARY_PATH: z.string().default('/usr/local/bin/amneziawg-go'),
  AWG_UNIT_FILE_PATH: z.string().default('/etc/systemd/system/route-awg@.service'),
  // S2S AWG3 mesh tunnel (front↔egress, second ring-relay hop) — deliberately a SEPARATE
  // interface/config from the client-facing AWG above (different key material, different peer
  // set, kernel-module-backed rather than amneziawg-go userspace). Reuses the SAME generic
  // route-awg@.service template unit (AWG_UNIT_FILE_PATH, %i-parameterized) — no separate unit
  // needed, systemctl enable/reload just targets route-awg@<this interface's basename> instead.
  MESH_AWG_CONFIG_PATH: z.string().default('/etc/amnezia/amneziawg/awgmesh0.conf'),
  CA_CERT_PATH: z.string().default('/etc/route-agent/certs/ca.crt'),
  AGENT_CERT_PATH: z.string().default('/etc/route-agent/certs/agent.crt'),
  AGENT_KEY_PATH: z.string().default('/etc/route-agent/certs/agent.key'),
});

const parsed = configSchema.safeParse(process.env);
if (!parsed.success) {
  process.stderr.write('❌ Invalid agent environment variables: ' + JSON.stringify(parsed.error.format(), null, 2) + '\n');
  process.exit(1);
}

export const config = parsed.data;
export { configSchema };
