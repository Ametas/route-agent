import crypto from 'crypto';
import { config } from '../config.js';

/**
 * Защищённое сравнение токенов через timingSafeEqual против Timing Attacks
 */
export function verifySecret(token: string | undefined | null): boolean {
  if (!token || typeof token !== 'string') return false;
  const expected = config.EGRESS_CONTROL_SECRET || '';
  const tokenBuf = Buffer.from(token, 'utf-8');
  const expectedBuf = Buffer.from(expected, 'utf-8');

  if (tokenBuf.length !== expectedBuf.length || expectedBuf.length === 0) {
    return false;
  }

  return crypto.timingSafeEqual(tokenBuf, expectedBuf);
}

/**
 * Извлечение секрета из gRPC metadata
 */
export function extractSecretFromMetadata(call: { metadata?: { get(key: string): unknown[] } }): string {
  const metadataValues = call.metadata ? call.metadata.get('x-orchestrator-secret') : [];
  return metadataValues && metadataValues[0] ? String(metadataValues[0]) : '';
}

/**
 * Комплексная проверка авторизации вызова gRPC (metadata или payload)
 */
export function authenticateCall(call: any): boolean {
  const metadataSecret = extractSecretFromMetadata(call);
  if (verifySecret(metadataSecret)) {
    return true;
  }

  const payloadSecret = call.request?.orchestratorSecret || call.request?.orchestrator_secret;
  return verifySecret(payloadSecret);
}
