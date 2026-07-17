import dotenv from 'dotenv';
import { z } from 'zod';

dotenv.config();

const configSchema = z.object({
  PORT: z.coerce.number().default(8081), // Ожидаемый gRPC порт управления нодой
  HOST: z.string().default('0.0.0.0'),
  EGRESS_CONTROL_SECRET: z.string(),     // Уникальный токен ноды (верифицируется через gRPC Metadata)
  SINGBOX_CONFIG_PATH: z.string().default('/etc/sing-box/config.json'),
  RELOAD_COMMAND: z.string().default('systemctl reload sing-box'),
});

const parsed = configSchema.safeParse(process.env);
if (!parsed.success) {
  process.stderr.write('❌ Invalid agent environment variables: ' + JSON.stringify(parsed.error.format(), null, 2) + '\n');
  process.exit(1);
}

export const config = parsed.data;
