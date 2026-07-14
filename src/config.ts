import dotenv from 'dotenv';
import { z } from 'zod';

dotenv.config();

const configSchema = z.object({
  PORT: z.coerce.number().default(8081),
  HOST: z.string().default('0.0.0.0'),
  EGRESS_CONTROL_SECRET: z.string(), // Обязательный секрет для авторизации пушей
  SINGBOX_CONFIG_PATH: z.string().default('/etc/sing-box/config.json'),
  RELOAD_COMMAND: z.string().default('systemctl reload sing-box'),
});

const parsed = configSchema.safeParse(process.env);
if (!parsed.success) {
  console.error('❌ Invalid environment variables:', JSON.stringify(parsed.error.format(), null, 2));
  process.exit(1);
}

export const config = parsed.data;
