import Fastify from 'fastify';
import { exec } from 'child_process';
import { promisify } from 'util';
import * as fs from 'fs/promises';
import path from 'path';
import { config } from './config.js';

const execAsync = promisify(exec);
export const server = Fastify({ logger: true });

// Эндпоинт для применения конфигурации
server.post('/agent/config', async (request, reply) => {
  const secretHeader = request.headers['x-orchestrator-secret'];

  // 1. Авторизация по секретному токену
  if (!secretHeader || secretHeader !== config.EGRESS_CONTROL_SECRET) {
    request.log.warn({ ip: request.ip }, 'Unauthorized config push attempt blocked');
    return reply.status(401).send({
      success: false,
      error: 'Unauthorized',
      message: 'Invalid orchestrator secret token.'
    });
  }

  const configObj = request.body;
  if (!configObj || typeof configObj !== 'object' || Array.isArray(configObj)) {
    return reply.status(400).send({
      success: false,
      error: 'BadRequest',
      message: 'Payload body must be a valid JSON object.'
    });
  }

  try {
    request.log.info('Received configuration update. Validating target directory...');
    
    // Гарантируем существование директории назначения (например, /etc/sing-box/)
    const dir = path.dirname(config.SINGBOX_CONFIG_PATH);
    await fs.mkdir(dir, { recursive: true });

    // 2. Атомарно перезаписываем файл конфигурации (запись во временный файл + rename)
    const targetDir = path.dirname(config.SINGBOX_CONFIG_PATH);
    const tempFilePath = path.join(targetDir, `.config.${Date.now()}.tmp`);

    request.log.info({ tempFilePath, targetPath: config.SINGBOX_CONFIG_PATH }, 'Writing configuration atomically');
    
    // Записываем во временный файл
    await fs.writeFile(tempFilePath, JSON.stringify(configObj, null, 2), 'utf-8');
    
    // Атомарно переименовываем/перемещаем поверх старого файла
    await fs.rename(tempFilePath, config.SINGBOX_CONFIG_PATH);

    // 3. Выполняем мягкую перезагрузку sing-box
    request.log.info({ command: config.RELOAD_COMMAND }, 'Executing sing-box reload command');
    const { stdout, stderr } = await execAsync(config.RELOAD_COMMAND);
    
    if (stdout) request.log.info({ stdout }, 'Reload command stdout');
    if (stderr) request.log.warn({ stderr }, 'Reload command stderr');

    return reply.status(200).send({
      success: true,
      message: 'Configuration successfully updated and sing-box reloaded.'
    });
  } catch (err: any) {
    request.log.error({ err }, 'Failed to apply configuration and reload sing-box');
    return reply.status(500).send({
      success: false,
      error: 'InternalServerError',
      message: err.message || 'Failed to update configuration.'
    });
  }
});

// Простой эндпоинт пинга для проверки здоровья ноды
server.get('/agent/ping', async () => {
  return { status: 'online', timestamp: new Date().toISOString() };
});

const start = async () => {
  try {
    const address = await server.listen({ port: config.PORT, host: config.HOST });
    server.log.info(`📡 Route Agent actively listening at ${address}`);
  } catch (err) {
    server.log.error(err);
    process.exit(1);
  }
};

if (process.env.NODE_ENV !== 'test') {
  start();
}
