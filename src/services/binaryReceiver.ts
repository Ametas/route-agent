import { createWriteStream } from 'fs';
import * as fs from 'fs/promises';
import crypto from 'crypto';
import type { ServerReadableStream, sendUnaryData } from '@grpc/grpc-js';
import pino from 'pino';
import { verifySecret, extractSecretFromMetadata } from '../middleware/auth.js';

const logger = pino({ level: 'info' });

/**
 * Приёмная часть клиентского стрима с бинарём — общая для всех загрузок.
 *
 * **ЗАЧЕМ ВЫНЕСЕНО.** В `binary.service.ts` эта механика написана ЧЕТЫРЕЖДЫ (sing-box, awg-tools,
 * awg-go, caddy) и ещё раз в `olcrtc.service.ts` — по сотне строк на копию, различающихся только
 * тем, что делают с готовым файлом. Решение здесь принимается одно и то же: как проверить секрет,
 * куда копить чанки, что считать пустой загрузкой, что подчистить при обрыве. Пятая копия
 * означала бы, что расхождение между ними станет вопросом времени, причём ТИХОЕ: одна ветка просто
 * начнёт вести себя иначе, чем остальные, и всплывёт это при разборе инцидента.
 *
 * **ЧТО СЮДА НЕ ПЕРЕЕХАЛО и почему.** Всё, что происходит с бинарём ПОСЛЕ приёма: setcap, symlink,
 * создание юнита, сборка DKMS, перезапуск служб. У потребителей это разное по сути, а не по форме —
 * разные последствия отказа и разная цена ошибки. Хелпер, который знает про systemd и capabilities,
 * был бы уже не приёмником, а складом чужих доменов.
 *
 * ⚠️ Существующие четыре обработчика на него ПОКА НЕ переведены: они лежат на пути раскатки, и
 * трогать их заодно с вводом mihomo значило бы смешать в одном изменении новую функциональность и
 * рефакторинг того, что работает. Перевод записан в беклог отдельной задачей — при следующей правке
 * любого из них переводить, а не копировать.
 */

export interface ReceivedBinary {
  /** Временный файл с принятым содержимым. Удалять его — забота вызывающего. */
  tempPath: string;
  /** Версия, объявленная отправителем; `'unknown'`, если не пришла. */
  version: string;
  /** Дискриминатор внутри одного RPC (например, `awg-quick` против `awg`). Пустая строка, если не задан. */
  targetBinary: string;
  bytes: number;
}

export interface ReceiveOptions {
  /** Имя RPC — только для логов, чтобы отказ было видно по имени вызова. */
  rpcName: string;
  /** Префикс временного файла в `/tmp`. */
  tempPrefix: string;
}

/**
 * Принимает стрим и передаёт готовый файл в `apply`.
 *
 * Секрет проверяется и в метаданных, и в первом чанке: клиенты шлют его по-разному, а отказ должен
 * быть одинаковым. При неверном секрете стрим не просто отвечает отказом, но и рвётся — иначе
 * отправитель продолжал бы лить байты в никуда.
 */
export async function receiveStreamedBinary(
  call: ServerReadableStream<any, any>,
  callback: sendUnaryData<any>,
  options: ReceiveOptions,
  apply: (received: ReceivedBinary) => Promise<{ success: boolean; message: string }>
): Promise<void> {
  const metadataSecret = extractSecretFromMetadata(call);
  let secretVerified = verifySecret(metadataSecret);
  let isAborted = false;

  const uniqueId = `${Date.now()}_${crypto.randomUUID().slice(0, 8)}`;
  const tempPath = `/tmp/${options.tempPrefix}_${uniqueId}.tmp`;
  let version = 'unknown';
  let targetBinary = '';
  let bytesWritten = 0;
  let fileStream: ReturnType<typeof createWriteStream> | null = null;

  const cleanup = () => {
    if (fileStream) fileStream.end();
    fs.unlink(tempPath).catch(() => {});
  };

  call.on('data', (data: any) => {
    if (isAborted) return;

    if (!secretVerified && verifySecret(data?.orchestratorSecret || data?.orchestrator_secret)) {
      secretVerified = true;
    }
    if (!secretVerified) {
      isAborted = true;
      logger.warn(`Unauthorized ${options.rpcName} attempt rejected`);
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (data.version) version = data.version;
    if (data.targetBinary || data.target_binary) targetBinary = data.targetBinary || data.target_binary;

    if (data.chunk && data.chunk.length > 0) {
      if (!fileStream) fileStream = createWriteStream(tempPath);
      const buf = Buffer.from(data.chunk);
      fileStream.write(buf);
      bytesWritten += buf.length;
    }
  });

  call.on('end', async () => {
    if (isAborted) return;

    if (fileStream) {
      await new Promise<void>((resolve) => fileStream!.end(resolve));
    }

    if (!secretVerified) {
      isAborted = true;
      await fs.unlink(tempPath).catch(() => {});
      logger.warn(`Unauthorized ${options.rpcName} attempt rejected`);
      try {
        callback(null, { success: false, message: 'Invalid orchestrator secret token.' });
      } catch {}
      call.destroy(new Error('PermissionDenied: Invalid orchestrator secret token.'));
      return;
    }

    if (bytesWritten === 0) {
      await fs.unlink(tempPath).catch(() => {});
      return callback(null, { success: false, message: 'No binary data received.' });
    }

    // Уборка идёт ДО ответа, а не в finally после него. Разница не косметическая: ответ
    // «готово» должен означать, что временного файла уже нет. В finally он выполнялся после
    // callback, и вызывающий видел успех, пока файл ещё лежал в /tmp — а на узле это десятки
    // мегабайт содержимого бинаря, оставленные там, где его никто не ждёт.
    let result: { success: boolean; message: string };
    try {
      result = await apply({ tempPath, version, targetBinary, bytes: bytesWritten });
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : 'Unknown error';
      logger.error({ err: msg, rpc: options.rpcName }, 'Failed to apply uploaded binary');
      result = { success: false, message: `Failed to upload binary: ${msg}` };
    }
    await fs.unlink(tempPath).catch(() => {});
    return callback(null, result);
  });

  call.on('error', (err) => {
    logger.error({ err: err.message }, `Error in ${options.rpcName} stream`);
    cleanup();
  });
  call.on('cancelled', cleanup);
}
