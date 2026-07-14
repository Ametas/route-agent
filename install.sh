#!/bin/bash
set -e

echo "⚙️ Starting Route Agent Installation..."

# Проверяем, запущен ли скрипт от root
if [ "$EUID" -ne 0 ]; then
  echo "❌ Please run as root (sudo)"
  exit 1
fi

# Парсим аргументы
SECRET=""
PORT="8081"
REPO="https://github.com/YOUR_GITHUB_USERNAME/route-agent.git"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --secret) SECRET="$2"; shift ;;
        --port) PORT="$2"; shift ;;
        --repo) REPO="$2"; shift ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

if [ -z "$SECRET" ]; then
  echo "❌ Error: --secret parameter is required."
  echo "Usage: ./install.sh --secret \"YOUR_SECRET\" [--port 8081] [--repo \"YOUR_REPO_URL\"]"
  exit 1
fi

# Определение рабочей директории
if [ -f "package.json" ] && [ -d "src" ]; then
  AGENT_DIR=$(pwd)
  echo "✅ Running inside existing project directory: $AGENT_DIR"
else
  echo "📦 Project files not found locally. Installing git and cloning repository..."
  if ! command -v git &> /dev/null; then
    if command -v apt-get &> /dev/null; then
      apt-get update && apt-get install -y git
    elif command -v yum &> /dev/null; then
      yum install -y git
    else
      echo "❌ Package manager not supported. Please install git manually."
      exit 1
    fi
  fi
  
  # Клонируем репозиторий
  rm -rf /opt/route-agent
  git clone "$REPO" /opt/route-agent
  cd /opt/route-agent
  AGENT_DIR="/opt/route-agent"
fi

# 1. Установка Node.js (если не установлен)
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

# 2. Установка зависимостей и сборка
echo "📦 Installing dependencies and compiling agent..."
if [ -f "package-lock.json" ]; then
  npm ci
else
  npm install
fi
npm run build

# 3. Генерация .env файла
echo "📝 Creating environment configuration..."
cat <<EOT > "$AGENT_DIR/.env"
PORT=$PORT
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=$SECRET
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
EOT

# 4. Регистрация демона в systemd
echo "🔄 Registering Route Agent as systemd service..."

cat <<EOT > /etc/systemd/system/route-agent.service
[Unit]
Description=Route Egress Agent Service
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

echo "🎉 Route Agent successfully installed and running on port $PORT!"
echo "📡 Add this node IP and port $PORT to your Route Orchestrator Control Plane."
