import * as fs from 'fs/promises';
import path from 'path';
import {
  credentials,
  loadPackageDefinition,
  Client as GrpcClient,
} from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import pino from 'pino';
import { config } from '../config.js';

const logger = pino({ level: 'info' });

// Separate from proto/agent.proto's own protoLoader.loadSync call in src/index.ts — this is a
// DIFFERENT gRPC target (sing-box's own local stats service, not our orchestrator<->agent
// contract). Loaded lazily/once, not at module-import top level like agent.proto, since this repo
// might run on a node with no tuic/hy2Sb configured at all (no v2ray_api listener to ever call).
const STATS_PROTO_PATH = path.resolve(process.cwd(), 'proto/singbox-stats.proto');
let statsPackageCache: any = null;

function loadStatsPackage(): any {
  if (statsPackageCache) return statsPackageCache;
  // `longs: String` — same real gotcha already hit once on the orchestrator side (int64 `value`
  // arrives as a numeric STRING off the wire, not a number; parseUserTrafficStats below always
  // Number(...)s it, never trusts the TS type alone).
  const packageDefinition = protoLoader.loadSync(STATS_PROTO_PATH, {
    keepCase: false,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
  });
  statsPackageCache = loadPackageDefinition(packageDefinition) as any;
  return statsPackageCache;
}

export interface SingBoxUserTrafficDelta {
  userUuid: string;
  uplinkBytes: number;
  downlinkBytes: number;
}

/**
 * Reads the CURRENTLY-APPLIED sing-box config off disk (the same file atomicApplyAndReload in
 * utils/singbox.ts writes) and extracts `experimental.v2ray_api.listen` — deliberately not a
 * hardcoded/env-configured port shared between this repo and route-orchestrator: the port is
 * whatever the orchestrator's egressSingbox.ts generator chose for THIS node, and route-agent
 * already has the exact same config file locally as the single source of truth for it. Returns
 * null (not throw) when the block is missing — a node with no tuic/hy2Sb configured at all
 * legitimately never gets a v2ray_api listener.
 */
export async function readLiveV2RayApiListenAddress(
  configPath: string = config.SINGBOX_CONFIG_PATH
): Promise<string | null> {
  try {
    const raw = await fs.readFile(configPath, 'utf-8');
    const parsed = JSON.parse(raw);
    const listen = parsed?.experimental?.v2ray_api?.listen;
    return typeof listen === 'string' && listen.trim() !== '' ? listen.trim() : null;
  } catch (err: unknown) {
    const msg = err instanceof Error ? err.message : String(err);
    logger.debug({ err: msg }, 'Could not read live sing-box config to resolve v2ray_api listen address');
    return null;
  }
}

/**
 * Stat names sing-box emits follow the same `user>>>{name}>>>traffic>>>uplink`/`downlink`
 * convention as upstream v2ray-core/xray-core — see stats.go on their side. `{name}` is exactly
 * the tuic/hysteria2 inbound `users[].name` value egressSingbox.ts sets to our own user uuid, so
 * no separate id-resolution step is needed here (unlike Remnawave's numeric userId elsewhere in
 * this project) — the name IS the uuid already.
 */
export function parseUserTrafficStats(stats: { name: string; value: unknown }[]): Map<string, { uplinkBytes: number; downlinkBytes: number }> {
  const byUser = new Map<string, { uplinkBytes: number; downlinkBytes: number }>();

  for (const stat of stats) {
    const parts = stat.name.split('>>>');
    if (parts.length !== 4 || parts[0] !== 'user' || parts[2] !== 'traffic') continue;
    const userUuid = parts[1];
    const direction = parts[3];
    if (direction !== 'uplink' && direction !== 'downlink') continue;

    const value = Number(stat.value);
    if (Number.isNaN(value)) continue;

    const entry = byUser.get(userUuid) ?? { uplinkBytes: 0, downlinkBytes: 0 };
    if (direction === 'uplink') entry.uplinkBytes += value;
    else entry.downlinkBytes += value;
    byUser.set(userUuid, entry);
  }

  return byUser;
}

/**
 * Dials sing-box's own local StatsService (loopback-only, no auth — see readLiveV2RayApiListenAddress's
 * doc comment for why that's an acceptable posture here) and returns per-user traffic DELTAS since
 * the last successful query (`reset: true` — sing-box zeroes each matched counter atomically as
 * part of returning its value, so there's no cumulative state to track on either side; a missed
 * poll just means that window's bytes are lost, the same "probabilistic, not transactional" posture
 * this whole telemetry surface already has elsewhere in this project).
 */
export async function queryUserTrafficDeltas(listenAddress: string): Promise<SingBoxUserTrafficDelta[]> {
  const statsPackage = loadStatsPackage();
  const client = new statsPackage.experimental.v2rayapi.StatsService(
    listenAddress,
    credentials.createInsecure()
  ) as GrpcClient & {
    queryStats(
      request: { patterns: string[]; reset: boolean; regexp: boolean },
      callback: (err: Error | null, response: { stat: { name: string; value: unknown }[] }) => void
    ): void;
  };

  try {
    const response = await new Promise<{ stat: { name: string; value: unknown }[] }>((resolve, reject) => {
      client.queryStats({ patterns: ['user>>>'], reset: true, regexp: false }, (err, res) => {
        if (err) reject(err);
        else resolve(res);
      });
    });

    const byUser = parseUserTrafficStats(response.stat ?? []);
    return Array.from(byUser.entries()).map(([userUuid, delta]) => ({ userUuid, ...delta }));
  } finally {
    client.close();
  }
}
