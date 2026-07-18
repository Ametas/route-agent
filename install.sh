#!/bin/bash
set -e

echo "⚙️ Starting Modernized gRPC Route Agent Installation..."

# Проверяем, запущен ли скрипт от root
if [ "$EUID" -ne 0 ]; then
  echo "❌ Please run as root (sudo)"
  exit 1
fi

# Парсим аргументы
SECRET=""
PORT="8081"
REPO="https://github.com/YOUR_GITHUB_USERNAME/route-agent.git"
OLCRTC_USER=""
OLCRTC_PASS=""
OLCRTC_PORT="8888"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --secret) SECRET="$2"; shift ;;
        --port) PORT="$2"; shift ;;
        --repo) REPO="$2"; shift ;;
        --olcrtc-user) OLCRTC_USER="$2"; shift ;;
        --olcrtc-pass) OLCRTC_PASS="$2"; shift ;;
        --olcrtc-port) OLCRTC_PORT="$2"; shift ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

if [ -z "$SECRET" ]; then
  echo "❌ Error: --secret parameter is required."
  echo "Usage: ./install.sh --secret \"YOUR_SECRET\" [--port 8081] [--repo \"YOUR_REPO_URL\"] [--olcrtc-user \"USER\"] [--olcrtc-pass \"PASS\"] [--olcrtc-port 8888]"
  exit 1
fi

# 1. Установка системных утилит
echo "📦 Updating package lists and installing system utilities (iptables, iproute2, sqlite3, git, curl)..."
apt-get update
apt-get install -y iptables iproute2 sqlite3 git curl

# 2. Provisioning бинарников WebRTC-слоя (olcrtc-manager и olcrtc)
echo "📥 Downloading and provisioning WebRTC components..."
TMP_DIR=$(mktemp -d)
echo "📁 Created temporary directory at $TMP_DIR"

# GitHub download URL placeholders
OLCRTC_MANAGER_URL="https://github.com/placeholder-org/olcrtc-manager/releases/latest/download/olcrtc-manager-linux-amd64.tar.gz"
OLCRTC_DAEMON_URL="https://github.com/placeholder-org/olcrtc/releases/latest/download/olcrtc-linux-amd64.tar.gz"

echo "⬇️ Downloading olcrtc-manager from $OLCRTC_MANAGER_URL..."
curl -L -s -o "$TMP_DIR/olcrtc-manager.tar.gz" "$OLCRTC_MANAGER_URL"

echo "⬇️ Downloading olcrtc daemon from $OLCRTC_DAEMON_URL..."
curl -L -s -o "$TMP_DIR/olcrtc.tar.gz" "$OLCRTC_DAEMON_URL"

echo "📦 Extracting olcrtc-manager..."
tar -xzf "$TMP_DIR/olcrtc-manager.tar.gz" -C "$TMP_DIR"

echo "📦 Extracting olcrtc daemon..."
tar -xzf "$TMP_DIR/olcrtc.tar.gz" -C "$TMP_DIR"

echo "⚙️ Moving WebRTC binaries to /usr/local/bin/..."
# Robust search and move using find in case of custom subdirectory structure inside archive
MANAGER_BIN=$(find "$TMP_DIR" -type f -name "olcrtc-manager" | head -n 1)
if [ -n "$MANAGER_BIN" ]; then
  mv "$MANAGER_BIN" /usr/local/bin/olcrtc-manager
else
  echo "❌ Error: olcrtc-manager binary not found in the archive"
  exit 1
fi

DAEMON_BIN=$(find "$TMP_DIR" -type f -name "olcrtc" | head -n 1)
if [ -n "$DAEMON_BIN" ]; then
  mv "$DAEMON_BIN" /usr/local/bin/olcrtc
else
  echo "❌ Error: olcrtc binary not found in the archive"
  exit 1
fi

echo "🔑 Setting executable permissions..."
chmod +x /usr/local/bin/olcrtc-manager
chmod +x /usr/local/bin/olcrtc

echo "🧹 Cleaning up temporary directory..."
rm -rf "$TMP_DIR"

echo "🔍 Verifying binaries accessibility..."
if command -v olcrtc-manager &> /dev/null && command -v olcrtc &> /dev/null; then
  echo "✅ WebRTC binaries successfully installed and verified in system PATH:"
  echo "   - olcrtc-manager: $(which olcrtc-manager)"
  echo "   - olcrtc: $(which olcrtc)"
else
  echo "❌ Error: Installed binaries are not accessible in the system PATH"
  exit 1
fi

# Определение рабочей директории
if [ -f "package.json" ] && [ -d "src" ]; then
  AGENT_DIR=$(pwd)
  echo "✅ Running inside existing project directory: $AGENT_DIR"
else
  echo "📦 Project files not found locally. Cloning repository..."
  # Клонируем репозиторий
  rm -rf /opt/route-agent
  git clone "$REPO" /opt/route-agent
  cd /opt/route-agent
  AGENT_DIR="/opt/route-agent"
fi

# 3. Установка Node.js (если не установлен)
if ! command -v node &> /dev/null; then
  echo "📦 Node.js not found. Installing Node.js 22 LTS..."
  curl -fsSL https://fnm.vercel.app/install | bash -s -- --skip-shell
  export PATH="/root/.local/share/fnm:$PATH"
  eval "`fnm env`"
  fnm install 22
  fnm use 22
  ln -sf "$(which node)" /usr/bin/node
  ln -sf "$(which npm)" /usr/bin/npm
else
  echo "✅ Node.js $(node -v) is already installed."
fi

# 4. Установка зависимостей и сборка бинарного gRPC-пакета
echo "📦 Installing gRPC dependencies and compiling agent..."
npm install
npm run build

# 5. Санити-чек структуры: проверяем, что папка proto скопирована
if [ ! -d "$AGENT_DIR/proto" ]; then
  echo "📦 Creating missing proto directory..."
  mkdir -p "$AGENT_DIR/proto"
fi

# 6. Генерация актуального .env файла окружения
echo "📝 Creating environment configuration..."
cat <<EOT > "$AGENT_DIR/.env"
PORT=$PORT
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=$SECRET
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
OLCRTC_USER=$OLCRTC_USER
OLCRTC_PASS=$OLCRTC_PASS
OLCRTC_PORT=$OLCRTC_PORT
EOT

# 7. Регистрация демона в systemd с root-привилегиями для перезагрузки sing-box и чтения /proc
echo "🔄 Registering Route Agent as systemd service..."

cat <<EOT > /etc/systemd/system/route-agent.service
[Unit]
Description=Route Egress gRPC Agent Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$AGENT_DIR
ExecStart=/usr/bin/node $AGENT_DIR/dist/index.js
Restart=always
RestartSec=5
EnvironmentFile=$AGENT_DIR/.env

[Install]
WantedBy=multi-user.target
EOT

systemctl daemon-reload
systemctl enable route-agent
systemctl restart route-agent

# 8. Регистрация и авто-настройка WebRTC-панели (olcrtc-manager) в systemd
echo "🔄 Registering olcrtc-manager as systemd service..."

cat <<EOT > /etc/systemd/system/olcrtc-manager.service
[Unit]
Description=WebRTC Panel Service (olcrtc-manager)
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/olcrtc-manager --port ${OLCRTC_PORT}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOT

echo "⚙️ Enabling and starting olcrtc-manager service..."
systemctl daemon-reload
systemctl enable --now olcrtc-manager

# Атомарный авто-сетап API
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "⏳ Waiting 3 seconds for olcrtc-manager API to start..."
  sleep 3

  echo "🔑 Auto-configuring WebRTC administrator account via local REST API..."
  if ! curl -s -f -X POST -H "Content-Type: application/json" \
    -d "{\"user\":\"$OLCRTC_USER\",\"password\":\"$OLCRTC_PASS\"}" \
    "http://127.0.0.1:${OLCRTC_PORT}/api/auth/setup"; then
    echo "❌ Error: Failed to perform auto-setup of WebRTC administrator account."
    echo "🧹 Rolling back olcrtc-manager service to prevent undefined state..."
    systemctl stop olcrtc-manager || true
    systemctl disable olcrtc-manager || true
    rm -f /etc/systemd/system/olcrtc-manager.service
    systemctl daemon-reload
    exit 1
  fi
  echo "✅ WebRTC administrator account successfully configured."
else
  echo "⚠️ WebRTC administrator credentials not provided (--olcrtc-user / --olcrtc-pass). Skipping API auto-setup."
fi

echo "🎉 Route Agent successfully upgraded to gRPC protocol and running on port $PORT!"
echo "📡 Link this node IP and port $PORT to your Route Orchestrator Control Plane securely."
echo "🌐 WebRTC Panel (olcrtc-manager) is running on port $OLCRTC_PORT."
