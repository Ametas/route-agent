import * as protoLoader from '@grpc/proto-loader';
import crypto from 'crypto';
import path from 'path';

export const PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');

let cachedProtoHash: string | null = null;

export function getCanonicalSchemaFromPackageDef(packageDefinition: any) {
  const messages: any[] = [];
  const services: any[] = [];

  for (const key of Object.keys(packageDefinition).sort()) {
    const item: any = packageDefinition[key];
    if (item.format === 'Protocol Buffer 3 DescriptorProto' || item.type?.field) {
      const rawFields = item.type?.field || [];
      const fields = rawFields
        .map((f: any) => ({
          number: f.number,
          name: f.name,
          type: f.type,
          label: f.label || 'LABEL_OPTIONAL',
          type_name: f.typeName || undefined,
        }))
        .sort((a: any, b: any) => a.number - b.number);

      messages.push({
        name: key,
        fields,
      });
    } else if (item.format === 'Protocol Buffer 3 ServiceDescriptorProto' || (item && typeof item === 'object')) {
      const methods: any[] = [];
      for (const mName of Object.keys(item).sort()) {
        const mDef = item[mName];
        if (mDef && typeof mDef === 'object' && mDef.path) {
          methods.push({
            name: mName,
            request_type: mDef.requestType?.type?.name || '',
            response_type: mDef.responseType?.type?.name || '',
            request_stream: Boolean(mDef.requestStream),
            response_stream: Boolean(mDef.responseStream),
          });
        }
      }
      if (methods.length > 0) {
        services.push({
          name: key,
          methods: methods.sort((a, b) => a.name.localeCompare(b.name)),
        });
      }
    }
  }

  messages.sort((a, b) => a.name.localeCompare(b.name));
  services.sort((a, b) => a.name.localeCompare(b.name));

  return { messages, services };
}

/**
 * Вычисляет sha256 хеш от канонического JSON-представления proto-схемы.
 * Результат кэшируется в памяти на весь жизненный цикл процесса.
 */
export function computeProtoContractHash(targetProtoPath: string = PROTO_PATH): string {
  if (cachedProtoHash && targetProtoPath === PROTO_PATH) {
    return cachedProtoHash;
  }

  const packageDefinition = protoLoader.loadSync(targetProtoPath, {
    keepCase: false,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
  });

  const canonicalSchema = getCanonicalSchemaFromPackageDef(packageDefinition);
  const canonicalJson = JSON.stringify(canonicalSchema);
  const hash = crypto.createHash('sha256').update(canonicalJson).digest('hex');

  if (targetProtoPath === PROTO_PATH) {
    cachedProtoHash = hash;
  }

  return hash;
}

/**
 * Функция сброса кэша хеша (используется в юнитах/тестировании при необходимости)
 */
export function clearProtoContractHashCache(): void {
  cachedProtoHash = null;
}
