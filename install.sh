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
DECOY_PORT="8443"
DOMAIN=""

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --secret) SECRET="$2"; shift ;;
        --port) PORT="$2"; shift ;;
        --repo) REPO="$2"; shift ;;
        --olcrtc-user) OLCRTC_USER="$2"; shift ;;
        --olcrtc-pass) OLCRTC_PASS="$2"; shift ;;
        --olcrtc-port) OLCRTC_PORT="$2"; shift ;;
        --decoy-port) DECOY_PORT="$2"; shift ;;
        --domain) DOMAIN="$2"; shift ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

if [ -z "$SECRET" ]; then
  echo "❌ Error: --secret parameter is required."
  echo "Usage: ./install.sh --secret \"YOUR_SECRET\" [--port 8081] [--repo \"YOUR_REPO_URL\"] [--domain \"domain.com\"] [--decoy-port 8443]"
  exit 1
fi

# 1. Установка системных утилит
echo "📦 Updating package lists and installing system utilities..."
apt-get update
apt-get install -y iptables iproute2 sqlite3 git curl unzip debian-keyring debian-archive-keyring apt-transport-https

# 2. Provisioning бинарников WebRTC-слоя (olcrtc)
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "📥 Downloading and provisioning Original olcrtc component..."
  TMP_DIR=$(mktemp -d)
  echo "📁 Created temporary directory at $TMP_DIR"

  OLCRTC_URL="https://github.com/openlibrecommunity/olcrtc/releases/latest/download/olcrtc-linux-amd64.tar.gz"

  echo "⬇️ Downloading olcrtc from $OLCRTC_URL..."
  if ! curl -L -s -f -o "$TMP_DIR/olcrtc.tar.gz" "$OLCRTC_URL"; then
    echo "❌ Error: Failed to download olcrtc binary from GitHub Releases."
    exit 1
  fi

  echo "📦 Extracting olcrtc architecture archive..."
  tar -xzf "$TMP_DIR/olcrtc.tar.gz" -C "$TMP_DIR"

  echo "⚙️ Moving olcrtc binary to /usr/local/bin/..."
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
else
  echo "⏭️ WebRTC credentials not provided. Skipping olcrtc components layer (Xeon Light mode active)..."
fi

# 3. Установка Node.js 22 LTS (Nodesource)
if ! command -v node &> /dev/null; then
  echo "📦 Node.js not found. Installing Node.js 22 LTS via Nodesource..."
  apt-get install -y ca-certificates gnupg
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
  echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" | tee /etc/apt/sources.list.d/nodesource.list
  apt-get update
  apt-get install -y nodejs
  echo "✅ Node.js $(node -v) successfully installed."
else
  echo "✅ Node.js $(node -v) is already installed."
fi

# Клонирование репозитория
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

# 8. Регистрация и авто-настройка WebRTC (olcrtc) в systemd
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "🔄 Registering olcrtc daemon engine as systemd service..."
  cat << EOT > /etc/systemd/system/olcrtc.service
[Unit]
Description=OpenLibreCommunity WebRTC Tunnel Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/olcrtc --port ${OLCRTC_PORT}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOT

  systemctl daemon-reload
  systemctl enable --now olcrtc

  echo "⏳ Waiting for local olcrtc REST API socket stabilization..."
  for i in {1..10}; do
    if curl -s -o /dev/null "http://127.0.0.1:${OLCRTC_PORT}/api/auth/me" || [ $i -eq 10 ]; then
      break
    fi
    sleep 1
  done

  curl -s -f --retry 3 --retry-delay 2 -X POST -H "Content-Type: application/json" \
    -d "{\"user\":\"$OLCRTC_USER\",\"password\":\"$OLCRTC_PASS\"}" \
    "http://127.0.0.1:${OLCRTC_PORT}/api/auth/setup"
  echo "✅ WebRTC administrator account successfully configured."
fi

# 9. УСТАНОВКА И НАСТРОЙКА CADDY ДЛЯ МАСКИРОВКИ (DECOY)
if [ -n "$DOMAIN" ]; then
  echo "📥 Domain provided. Installing official Caddy Server package..."
  
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update
  apt-get install -y caddy

  echo "📁 Setting up target directory for decoy..."
  mkdir -p /var/www/decoy

  # Проверяем наличие папки заглушки в репозитории и копируем её содержимое
  if [ -d "$AGENT_DIR/decoy" ]; then
    echo "📦 Extracting custom decoy template from repository folder..."
    cp -r "$AGENT_DIR/decoy/"* /var/www/decoy/
  elif [ -d "$AGENT_DIR/public" ]; then
    echo "📦 Extracting public assets as backup decoy template..."
    cp -r "$AGENT_DIR/public/"* /var/www/decoy/
  else
    echo "⚙️ No template folder found in repository. Creating operational fallback landing page..."
    echo "<html><body style='background:#070913;color:#fff;font-family:sans-serif;text-align:center;padding-top:20%;'><h1>Operations Command Center</h1><p>Status: Nominal</p></body></html>" > /var/www/decoy/index.html
  fi

  chown -R caddy:caddy /var/www/decoy

  echo "📝 Generating production Caddyfile configuration block..."
  cat << EOT > /etc/caddy/Caddyfile
$DOMAIN:$DECOY_PORT {
	handle {
		root * /var/www/decoy
		file_server
	}
}
EOT

  echo "🔄 Activating and starting Caddy proxy..."
  systemctl daemon-reload
  systemctl enable caddy
  systemctl restart caddy
  echo "✅ Caddy Server successfully configured with automated TLS on port $DECOY_PORT!"
else
  echo "⏭️ Domain parameter not provided. Skipping Caddy Server provisioning..."
fi

echo "---"
echo "🎉 Installation pass complete. Route Agent running on port $PORT!"
[ -n "$DOMAIN" ] && echo "🌐 Stealth Deflection active: https://$DOMAIN:$DECOY_PORT mapping to /var/www/decoy"