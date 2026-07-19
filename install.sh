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

# 1.5 Автоматическая настройка фаервола UFW
echo "🛡️ Checking firewall status (UFW)..."
if ! command -v ufw &> /dev/null; then
  echo "📦 UFW is not installed. Installing and securing UFW layer..."
  apt-get install -y ufw
  
  echo "🔑 Allowing standard SSH port 22 to prevent lockout..."
  ufw allow 22/tcp
  
  echo "🚀 Enabling UFW daemon..."
  ufw --force enable
else
  echo "✅ UFW firewall is already installed."
fi

echo "⚙️ Verifying gRPC Route Agent ports in UFW..."
if ! ufw status | grep -q "$PORT"; then
  echo "🔓 Opening gRPC port $PORT..."
  ufw allow "$PORT"
else
  echo "✅ gRPC port $PORT is already open."
fi

if ! ufw status | grep -q "443"; then
  echo "🔓 Opening default HTTPS port 443 for sing-box transit and Caddy decoy..."
  ufw allow 443
else
  echo "✅ Port 443 is already open."
fi

# Если это НЕ Зеон нода (переданы WebRTC креды), проверяем и открываем olcrtc порт
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "⚙️ Verifying WebRTC layer ports (Olcrtc)..."
  if ! ufw status | grep -q "$OLCRTC_PORT"; then
    echo "🔓 Opening WebRTC Olcrtc management port $OLCRTC_PORT..."
    ufw allow "$OLCRTC_PORT"
  else
    echo "✅ Olcrtc management port $OLCRTC_PORT is already open."
  fi
fi

echo "🔄 Reloading UFW firewall rules..."
ufw reload

# Разрешаем gRPC Route Agent'у беспарольно вызывать ufw через sudo,
# чтобы он мог динамически открывать/закрывать порты Hy2/TUIC без интерактивного запроса пароля.
echo "🔐 Configuring passwordless sudoers rule for UFW management (route-agent-ufw)..."
cat > /etc/sudoers.d/route-agent-ufw <<'EOF'
ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw
EOF
chmod 0440 /etc/sudoers.d/route-agent-ufw

# 2. Установка бинарного ядра sing-box и создание его службы
if ! command -v sing-box &> /dev/null; then
  echo "📦 sing-box core not found. Provisioning official sing-box 1.12.0 binary..."
  TMP_SB=$(mktemp -d)
  curl -Lo "$TMP_SB/sing-box.tar.gz" https://github.com/SagerNet/sing-box/releases/download/v1.12.0/sing-box-1.12.0-linux-amd64.tar.gz
  tar -xzf "$TMP_SB/sing-box.tar.gz" -C "$TMP_SB" --strip-components=1
  mv "$TMP_SB/sing-box" /usr/local/bin/sing-box
  chmod +x /usr/local/bin/sing-box
  rm -rf "$TMP_SB"
  
  setcap 'cap_net_admin,cap_net_bind_service=+ep' /usr/local/bin/sing-box
  mkdir -p /etc/sing-box
  echo '{"route":{"rules":[]}}' > /etc/sing-box/config.json

  cat << EOT > /etc/systemd/system/sing-box.service
  [Unit]
  Description=sing-box service
  After=network.target nss-lookup.target

  [Service]
  CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
  AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
  ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json
  Restart=always
  RestartSec=5
  # Исправленная безопасная строка для релоада через шелл:
  ExecReload=/bin/sh -c "/usr/local/bin/sing-box check -c /etc/sing-box/config.json && /bin/kill -HUP \$MAINPID"

  [Install]
  WantedBy=multi-user.target
  EOT

  systemctl daemon-reload
  systemctl enable --now sing-box
  echo "✅ sing-box core successfully initialized."
else
  echo "✅ sing-box core is already installed."
fi

# 3. Provisioning оригинального бинарника olcrtc (только для зарубежных нод)
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
  echo "📥 Downloading and provisioning Original olcrtc component..."
  TMP_DIR=$(mktemp -d)
  OLCRTC_URL="https://github.com/openlibrecommunity/olcrtc/releases/latest/download/olcrtc-linux-amd64.tar.gz"

  if curl -L -s -f -o "$TMP_DIR/olcrtc.tar.gz" "$OLCRTC_URL"; then
    tar -xzf "$TMP_DIR/olcrtc.tar.gz" -C "$TMP_DIR"
    REAL_BIN=$(find "$TMP_DIR" -type f -name "olcrtc" | head -n 1)
    if [ -n "$REAL_BIN" ]; then
      mv "$REAL_BIN" /usr/local/bin/olcrtc
      chmod +x /usr/local/bin/olcrtc
    fi
  fi
  rm -rf "$TMP_DIR"
else
  echo "⏭️ WebRTC credentials not provided. Skipping olcrtc components layer..."
fi

# 4. Установка Node.js 22 LTS (Nodesource)
if ! command -v node &> /dev/null; then
  echo "📦 Node.js not found. Installing Node.js 22 LTS via Nodesource..."
  apt-get install -y ca-certificates gnupg
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
  echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" | tee /etc/apt/sources.list.d/nodesource.list
  apt-get update
  apt-get install -y nodejs
else
  echo "✅ Node.js $(node -v) is already installed."
fi

# Клонирование репозитория агента
if [ ! -d "$AGENT_DIR" ]; then
  echo "📥 Cloning route-agent repository into $AGENT_DIR..."
  git clone "$REPO" "$AGENT_DIR"
fi
cd "$AGENT_DIR"

echo "📦 Installing Node.js dependencies and compiling agent..."
npm ci
npm run build

if [ ! -d "$AGENT_DIR/proto" ]; then
  mkdir -p "$AGENT_DIR/proto"
fi

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

# Регистрация и авто-настройка WebRTC в systemd
if [ -n "$OLCRTC_USER" ] && [ -n "$OLCRTC_PASS" ]; then
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

  for i in {1..10}; do
    if curl -s -o /dev/null "http://127.0.0.1:${OLCRTC_PORT}/api/auth/me" || [ $i -eq 10 ]; then
      break
    fi
    sleep 1
  done

  curl -s -f --retry 3 --retry-delay 2 -X POST -H "Content-Type: application/json" \
    -d "{\"user\":\"$OLCRTC_USER\",\"password\":\"$OLCRTC_PASS\"}" \
    "http://127.0.0.1:${OLCRTC_PORT}/api/auth/setup" || true
fi

# Настройка Caddy маскировки
if [ -n "$DOMAIN" ]; then
  echo "📥 Domain provided. Installing official Caddy Server package..."
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update
  apt-get install -y caddy

  mkdir -p /var/www/decoy
  if [ -d "$AGENT_DIR/decoy" ]; then
    cp -r "$AGENT_DIR/decoy/"* /var/www/decoy/
  else
    echo "<html><body style='background:#070913;color:#fff;font-family:sans-serif;text-align:center;padding-top:20%;'><h1>Operations Command Center</h1><p>Status: Nominal</p></body></html>" > /var/www/decoy/index.html
  fi
  chown -R caddy:caddy /var/www/decoy

  cat << EOT > /etc/caddy/Caddyfile
$DOMAIN:$DECOY_PORT {
	handle {
		root * /var/www/decoy
		file_server
	}
}
EOT

  systemctl daemon-reload
  systemctl enable caddy
  systemctl restart caddy
fi

echo "---"
echo "🎉 Installation complete. Route Agent running on port $PORT!"
[ -n "$DOMAIN" ] && echo "🌐 Stealth Deflection active: https://$DOMAIN:$DECOY_PORT"