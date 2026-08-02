import fs from 'fs';
import crypto from 'crypto';
import path from 'path';
import http from 'http';
import https from 'https';

const LOCAL_PROTO_PATH = path.resolve(process.cwd(), 'proto/agent.proto');

function getHash(content) {
  return crypto.createHash('sha256').update(content).digest('hex');
}

async function fetchRemoteProto(url) {
  return new Promise((resolve, reject) => {
    const client = url.startsWith('https') ? https : http;
    client.get(url, (res) => {
      if (res.statusCode && (res.statusCode < 200 || res.statusCode >= 300)) {
        reject(new Error(`Failed to fetch proto from ${url}. Status code: ${res.statusCode}`));
        return;
      }
      let data = '';
      res.on('data', chunk => { data += chunk; });
      res.on('end', () => resolve(data));
    }).on('error', reject);
  });
}

async function main() {
  if (!fs.existsSync(LOCAL_PROTO_PATH)) {
    console.error(`❌ Local proto file missing at ${LOCAL_PROTO_PATH}`);
    process.exit(1);
  }

  const localContent = fs.readFileSync(LOCAL_PROTO_PATH, 'utf-8');
  const localHash = getHash(localContent);

  const targetPath = process.env.ORCHESTRATOR_PROTO_PATH;
  const targetUrl = process.env.REMOTE_PROTO_URL;

  if (targetPath) {
    if (!fs.existsSync(targetPath)) {
      console.error(`❌ Target orchestrator proto file not found at ${targetPath}`);
      process.exit(1);
    }
    const targetContent = fs.readFileSync(targetPath, 'utf-8');
    const targetHash = getHash(targetContent);

    if (localHash !== targetHash) {
      console.error('❌ DISCREPANCY DETECTED: proto/agent.proto is out of sync with orchestrator proto!');
      console.error(`Local SHA256:  ${localHash}`);
      console.error(`Target SHA256: ${targetHash}`);
      process.exit(1);
    }
    console.log('✅ proto/agent.proto is perfectly in sync with orchestrator proto.');
    return;
  }

  if (targetUrl) {
    try {
      const targetContent = await fetchRemoteProto(targetUrl);
      const targetHash = getHash(targetContent);
      if (localHash !== targetHash) {
        console.error('❌ DISCREPANCY DETECTED: proto/agent.proto is out of sync with remote orchestrator proto!');
        console.error(`Local SHA256:  ${localHash}`);
        console.error(`Remote SHA256: ${targetHash}`);
        process.exit(1);
      }
      console.log('✅ proto/agent.proto is perfectly in sync with remote orchestrator proto.');
      return;
    } catch (err) {
      console.error(`❌ Failed to verify remote proto: ${err.message}`);
      process.exit(1);
    }
  }

  console.log(`ℹ️ Local proto file verified. SHA256: ${localHash}`);
  console.log('💡 Tip: Set ORCHESTRATOR_PROTO_PATH or REMOTE_PROTO_URL in CI environment to auto-verify sync against route-orchestrator.');
}

main().catch(err => {
  console.error(`❌ Proto sync check error: ${err.message}`);
  process.exit(1);
});
