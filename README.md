# Route Agent

Stealth Egress Node Agent for Route Orchestrator. 

This agent runs as a systemd service on a VPS, listens for sing-box configuration updates from the orchestrator, writes them to `/etc/sing-box/config.json`, and triggers a service reload.

## Features

- **Fastify Web Server**: High-performance, low-overhead HTTP server.
- **Secure by Design**: Checks authorization using the `x-orchestrator-secret` header.
- **Robust Configuration Validation**: Parses and validates incoming configurations before saving.
- **Auto-Reload**: Automatically triggers `systemctl reload sing-box` upon configuration updates.
- **TypeScript ESM**: Built with modern TypeScript strict rules and ESM exports.

## Prerequisites

- Node.js >= 20.0.0
- npm
- sing-box installed on the system (and manageable via systemd)

## Getting Started

### One-Click Remote Installation (Recommended)

Run the following command on a clean Ubuntu/Debian VPS to install Node.js, clone the repository, build the agent, and register it as a systemd service:

```bash
curl -fsSL https://raw.githubusercontent.com/YOUR_GITHUB_USERNAME/route-agent/main/install.sh | sudo bash -s -- --secret "YOUR_SECRET_TOKEN" [--port 8081] [--repo "YOUR_REPO_URL"]
```

### Local Manual Installation

If you have already cloned the repository on your VPS:

1. Run the install script directly:
   ```bash
   sudo ./install.sh --secret "YOUR_SECRET_TOKEN" [--port 8081]
   ```

### 2. Configuration

Create a `.env` file based on `.env.example`:

```bash
cp .env.example .env
```

Edit the `.env` file with your settings:

```ini
PORT=8081
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

## API Specification

### Health Check (Ping)

- **URL**: `/agent/ping`
- **Method**: `GET`
- **Response**:
  ```json
  {
    "status": "online",
    "timestamp": "2026-07-14T15:45:00.000Z"
  }
  ```

### Update Configuration

- **URL**: `/agent/config`
- **Method**: `POST`
- **Headers**:
  - `x-orchestrator-secret`: `<your_super_secure_secret_token>`
  - `Content-Type`: `application/json`
- **Request Body**:
  - A valid JSON object matching the sing-box configuration schema.
- **Response (Success 200)**:
  ```json
  {
    "success": true,
    "message": "Configuration successfully updated and sing-box reloaded."
  }
  ```
- **Response (Unauthorized 401)**:
  ```json
  {
    "success": false,
    "error": "Unauthorized",
    "message": "Invalid orchestrator secret token."
  }
  ```
- **Response (Bad Request 400)**:
  ```json
  {
    "success": false,
    "error": "BadRequest",
    "message": "Payload body must be a valid JSON object."
  }
  ```
