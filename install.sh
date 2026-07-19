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
REPO="https://github.com/Ametas/route-agent.git"
OLCRTC_USER=""
OLCRTC_PASS=""
OLCRTC_PORT="8888"
AGENT_DIR="/opt/route-agent"

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
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "📥 Downloading and provisioning Original olcrtc component..."
  TMP_DIR=$(mktemp -d)
  echo "📁 Created temporary directory at $TMP_DIR"

  # Ссылка на официальный стабильный релиз монолита olcrtc
  OLCRTC_URL="https://github.com/openlibrecommunity/olcrtc/releases/latest/download/olcrtc-linux-amd64.tar.gz"

  echo "⬇️ Downloading olcrtc from $OLCRTC_URL..."
  if ! curl -L -s -f -o "$TMP_DIR/olcrtc.tar.gz" "$OLCRTC_URL"; then
    echo "❌ Error: Failed to download olcrtc binary from GitHub Releases (404/Network failure)."
    exit 1
  fi

  echo "📦 Extracting olcrtc architecture archive..."
  tar -xzf "$TMP_DIR/olcrtc.tar.gz" -C "$TMP_DIR"

  echo "⚙️ Moving olcrtc binary to /usr/local/bin/..."
  # Ищем скомпилированный Go-бинарник внутри распакованной папки
  REAL_BIN=$(find "$TMP_DIR" -type f -name "olcrtc" | head -n 1)
  if [ -n "$REAL_BIN" ]; then
    mv "$REAL_BIN" /usr/local/bin/olcrtc
    chmod +x /usr/local/bin/olcrtc
  else
    echo "❌ Error: 'olcrtc' binary executable not found inside the downloaded archive."
    exit 1
  fi

  echo "🧹 Cleaning up temporary directory..."
  rm -rf "$TMP_DIR"

  echo "🔍 Verifying binary accessibility..."
  if command -v olcrtc &> /dev/null; then
    echo "✅ Original olcrtc successfully provisioned at: $(which olcrtc)"
  else
    echo "❌ Error: olcrtc binary is not accessible in system PATH."
    exit 1
  fi
else
  echo "⏭️ WebRTC credentials not provided. Skipping olcrtc components layer (Xeon Light mode active)..."
fi
# 3. Установка Node.js (если не установлен)
if ! command -v node &> /dev/null; then
  echo "📦 Node.js not found. Installing Node.js 22 LTS via FNM..."
  curl -fsSL https://fnm.vercel.app/install | bash -s -- --skip-shell
  
  # Прописываем пути FNM глобально для root пользователя (персистентность)
  echo 'export PATH="/root/.local/share/fnm:$PATH"' >> /root/.bashrc
  echo 'eval "`fnm env`"' >> /root/.bashrc
  
  export PATH="/root/.local/share/fnm:$PATH"
  eval "`fnm env`"
  
  fnm install 22
  fnm use 22
  
  # Создаем глобальные системные симлинки
  ln -sf "$(which node)" /usr/bin/node
  ln -sf "$(which npm)" /usr/bin/npm
else
  echo "✅ Node.js $(node -v) is already installed."
fi

if [ ! -d "$AGENT_DIR" ]; then
  echo "📥 Cloning route-agent repository into $AGENT_DIR..."
  git clone "$REPO" "$AGENT_DIR"
fi
cd "$AGENT_DIR"

# 4. Установка зависимостей и сборка проекта
echo "📦 Installing Node.js dependencies and compiling agent..."
npm ci
npm run build

# 5. Проверка структуры директории прототипов
if [ ! -d "$AGENT_DIR/proto" ]; then
  echo "📦 Creating missing proto directory..."
  mkdir -p "$AGENT_DIR/proto"
fi

# 6. Генерация .env файла окружения
echo "📝 Creating environment configuration..."
cat << EOT > "$AGENT_DIR/.env"
PORT=$PORT
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=$SECRET
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
OLCRTC_USER=$OLCRTC_USER
OLCRTC_PASS=$OLCRTC_PASS
OLCRTC_PORT=$OLCRTC_PORT
EOT

chmod 600 "$AGENT_DIR/.env"

# 7. Регистрация демона route-agent в systemd
echo "🔄 Registering Route Agent as systemd service..."

cat << EOT > /etc/systemd/system/route-agent.service
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
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "🔄 Registering olcrtc daemon engine as systemd service..."

  cat << EOT > /etc/systemd/system/olcrtc.service
[Unit]
Description=OpenLibreCommunity WebRTC Tunnel Service
After=network.target

[Service]
Type=simple
User=root
# Запускаем монолит, передавая ему порт веб-интерфейса и API управления
ExecStart=/usr/local/bin/olcrtc --port ${OLCRTC_PORT}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOT

  echo "⚙️ Enabling and starting olcrtc unit..."
  systemctl daemon-reload
  systemctl enable --now olcrtc

  echo "⏳ Waiting for local olcrtc REST API socket stabilization..."
  for i in {1..10}; do
    if curl -s -o /dev/null "http://127.0.0.1:${OLCRTC_PORT}/api/auth/me" || [ $i -eq 10 ]; then
      break
    fi
    sleep 1
  done

  echo "🔑 Auto-configuring administrator account via local REST API..."
  if ! curl -s -f --retry 3 --retry-delay 2 -X POST -H "Content-Type: application/json" \
    -d "{\"user\":\"$OLCRTC_USER\",\"password\":\"$OLCRTC_PASS\"}" \
    "http://127.0.0.1:${OLCRTC_PORT}/api/auth/setup"; then
    echo "❌ Error: Failed to perform auto-setup of olcrtc administrator account."
    systemctl stop olcrtc || true
    systemctl disable olcrtc || true
    rm -f /etc/systemd/system/olcrtc.service
    systemctl daemon-reload
    exit 1
  fi
  echo "✅ WebRTC administrator account successfully configured."
else
  echo "⚠️ WebRTC administrator credentials not provided. Skipping API auto-setup."
fi

echo "---"
echo "🎉 Route Agent successfully upgraded to gRPC protocol and running on port $PORT!"
echo "📡 Link this node IP and port $PORT to your Route Orchestrator Control Plane securely."
echo "🌐 WebRTC Panel (olcrtc-manager) is running on port $OLCRTC_PORT."