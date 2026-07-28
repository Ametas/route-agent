# Route Agent

Stealth Egress Node Agent for Route Orchestrator. 

This agent runs as a systemd service on a VPS, receives commands and streams telemetry from the orchestrator over gRPC (HTTP/2) with Mutual TLS (mTLS), manages sing-box, Caddy, AmneziaWG 3.0, olcrtc-manager, and firewall rules.

## Features

- **gRPC (HTTP/2) Server**: High-performance bi-directional and server-side streaming RPC interface.
- **Mutual TLS (mTLS) Security**: Strict client certificate verification using Root CA and secret authorization headers (`x-orchestrator-secret`).
- **Robust Configuration Validation & Rollback**: Validates incoming sing-box/Caddy configs with automatic rollback on reload failures.
- **Stream Uploads**: Efficient chunked streaming of binary updates (`sing-box`, `olcrtc`) to disk without RAM spikes.
- **Dynamic Telemetry**: Live stream of CPU, RAM, active connections, sing-box journal logs, WebRTC status, and AWG active peer count.
- **L3 & L7 Traffic Obfuscation**: Remote configuration of Caddy reverse proxy (VLESS XHTTP / gRPC) and AmneziaWG 3.0.
- **TypeScript ESM**: Built with modern TypeScript strict rules, ESM exports, and zero external shell injection vulnerabilities.

## Prerequisites

- Node.js >= 20.0.0
- npm
- sing-box installed on the system (and manageable via systemd)

## Getting Started

### One-Click Remote Installation (Recommended)

Run the following command on a clean Ubuntu/Debian VPS to install Node.js, clone the repository, build the agent, and register it as a systemd service:

```bash
curl -fsSL https://raw.githubusercontent.com/Ametas/route-agent/main/install.sh | sudo bash -s -- --secret "YOUR_SECRET_TOKEN" [--port 8083] [--repo "YOUR_REPO_URL"]
```

### Local Manual Installation

If you have already cloned the repository on your VPS:

1. Run the install script directly:
   ```bash
   sudo ./install.sh --secret "YOUR_SECRET_TOKEN" [--port 8083]
   ```

### 2. Configuration

Create a `.env` file based on `.env.example`:

```bash
cp .env.example .env
```

Edit the `.env` file with your settings:

```ini
PORT=8083
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=your_super_secure_secret_token
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
```

### 3. Build

Compile the TypeScript source files to JavaScript:

```bash
npm run build
```

The output will be placed in the `dist/` directory.

### 4. Running the Service

You can start the agent in production mode with:

```bash
npm start
```

For development:

```bash
npm run dev
```

To run the test suite:

```bash
npm run test
```

## Systemd Service Configuration

To run Route Agent as a system service on your VPS, create a systemd service file:

`/etc/systemd/system/route-agent.service`

```ini
[Unit]
Description=Route Egress Agent Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/route-agent
ExecStart=/usr/bin/node /opt/route-agent/dist/index.js
Restart=always
RestartSec=5
EnvironmentFile=/opt/route-agent/.env

[Install]
WantedBy=multi-user.target
```

Reload systemd daemon, enable, and start the service:

```bash
systemctl daemon-reload
systemctl enable route-agent
systemctl start route-agent
```

## API Specification & gRPC Architecture

Route Agent exposes a gRPC service defined in `proto/agent.proto` under the package `agent` (`EgressAgentService`).

All communication uses **gRPC over HTTP/2** with mandatory **mTLS** (Mutual TLS with Root CA verification) and secret header verification (`x-orchestrator-secret`).

### RPC Methods (`EgressAgentService`)

1. **`ApplyConfig (ConfigPayload) returns (ConfigResponse)`**
   - Applies and validates incoming JSON configuration for `sing-box`. Performs syntax check and atomic swap with fallback rollback on reload error.

2. **`StreamTelemetry (TelemetryRequest) returns (stream TelemetryResponse)`**
   - Server-writable streaming RPC delivering real-time telemetry ticks (CPU usage, RAM usage, active connections, system journal logs, sing-box version, AWG active peers count, and WebRTC status).

3. **`UpgradeSingbox (UpgradePayload) returns (UpgradeResponse)`**
   - Downloads sing-box binary from specified URL via safe `execFile` curl invocation, verifies syntax, and atomic-swaps the binary.

4. **`UploadSingboxBinary (stream BinaryChunkPayload) returns (UploadBinaryResponse)`**
   - Client streaming RPC for uploading a new `sing-box` binary in chunks. Chunks are streamed directly to disk to minimize RAM usage.

5. **`UploadOlcrtcBinary (stream BinaryChunkPayload) returns (UploadBinaryResponse)`**
   - Client streaming RPC for uploading `olcrtc` / `olcrtc-manager` component binaries in chunks.

6. **`ConfigureCaddy (CaddyConfigPayload) returns (CaddyConfigResponse)`**
   - Configures Caddy reverse proxy for VLESS XHTTP & gRPC transport, sets up camouflage HTML web pages, and reloads Caddy with config rollback.

7. **`ConfigureOlcrtc (OlcrtcConfigPayload) returns (OlcrtcConfigResponse)`**
   - Configures and manages systemd service for `olcrtc-manager` WebRTC service.

8. **`ConfigureAwg (AwgConfigPayload) returns (AwgConfigResponse)`**
   - Remote L3 interface configuration for AmneziaWG 3.0 (keys, obfuscation parameters `jc`, `jmin`, `jmax`, `s1-s4`, `h1-h4`, header protection, peers, and IPv6 isolation).

9. **`ManageFirewall (FirewallPayload) returns (FirewallResponse)`**
   - Dynamic firewall rule management for UFW and iptables (TCP/UDP open ports).

10. **`SelfUpdate (SelfUpdatePayload) returns (SelfUpdateResponse)`**
    - Triggers self-update sequence (`git fetch --all`, `git reset --hard @{u}`, `git clean -fd`, `npm ci`, `npm run build`, and `systemctl restart route-agent`).
