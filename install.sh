#!/bin/bash
set -e

echo "⚙️ Starting Lightweight Route Egress Agent Installation..."

if [ "$EUID" -ne 0 ]; then
  echo "❌ Please run as root (sudo)"
  exit 1
fi

SECRET=""
PORT="8081"
REPO="https://github.com/Ametas/route-agent.git"
AGENT_DIR="/opt/route-agent"
DOMAIN=""
DECOY_PORT="8443"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --secret) SECRET="$2"; shift ;;
        --port) PORT="$2"; shift ;;
        --repo) REPO="$2"; shift ;;
        --domain) DOMAIN="$2"; shift ;;
        --decoy-port) DECOY_PORT="$2"; shift ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

if [ -z "$SECRET" ]; then
  echo "❌ Error: --secret parameter is required."
  exit 1
fi

# 1. Установка системных зависимостей и подключение репозитория Caddy
echo "📦 Installing system packages and Caddy repository..."
apt-get update
apt-get install -y iptables iproute2 ufw git curl unzip debian-keyring debian-archive-keyring apt-transport-https ca-certificates gnupg

if ! command -v caddy &> /dev/null; then
  echo "📥 Adding Caddy official apt repository..."
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update
  apt-get install -y caddy
fi

# 2. Настройка UFW
ufw allow 22/tcp || true
ufw allow 443/tcp || true
ufw allow 443/udp || true
ufw allow "$PORT"/tcp || true
ufw --force enable || true

cat > /etc/sudoers.d/route-agent-ufw <<'EOF'
ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw
EOF
chmod 0440 /etc/sudoers.d/route-agent-ufw

# 3. Базовая заготовка путей sing-box и AmneziaWG (без создания сервиса sing-box —
# его systemd unit-файл провижинится лениво, идемпотентно, агентом самим (см.
# ensureSingboxSystemdUnit в src/utils/singbox.ts) в момент первой реальной загрузки
# бинарника sing-box от оркестратора, зеркально паттерну AWG/Olcrtc)
mkdir -p /etc/sing-box /var/www/decoy /var/lib/caddy /etc/amnezia/amneziawg
chmod 755 /var/lib/caddy || true
echo '{"route":{"rules":[]}}' > /etc/sing-box/config.json

# 4. Установка Node.js 22 LTS
if ! command -v node &> /dev/null; then
  mkdir -p /etc/apt/keyrings
  curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
  echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" | tee /etc/apt/sources.list.d/nodesource.list
  apt-get update
  apt-get install -y nodejs
fi

# 5. Клонирование и запуск агента
if [ ! -d "$AGENT_DIR" ]; then
  git clone "$REPO" "$AGENT_DIR"
fi
cd "$AGENT_DIR"

npm ci
npm run build

# Распаковка сертификатов mTLS (если передана зашифрованная строка в Base64)
CERTS_DIR="/etc/route-agent/certs"
mkdir -p "$CERTS_DIR"

IS_MTLS_PACKAGE=0
if command -v node &> /dev/null; then
  node -e '
    try {
      const raw = process.argv[1];
      const decoded = Buffer.from(raw, "base64").toString("utf-8");
      const json = JSON.parse(decoded);
      if (json && json.secret && json.ca && json.cert && json.key) {
        const fs = require("fs");
        fs.writeFileSync("/etc/route-agent/certs/ca.crt", json.ca);
        fs.writeFileSync("/etc/route-agent/certs/agent.crt", json.cert);
        fs.writeFileSync("/etc/route-agent/certs/agent.key", json.key);
        fs.chmodSync("/etc/route-agent/certs/agent.key", 0o600);
        process.stdout.write(json.secret);
        process.exit(0);
      }
    } catch {}
    process.exit(1);
  ' "$SECRET" > /tmp/route-agent-secret.tmp 2>/dev/null && IS_MTLS_PACKAGE=1 || true

  if [ "$IS_MTLS_PACKAGE" -eq 1 ]; then
    echo "🔒 Unpacked mTLS certificates and secret into /etc/route-agent/certs/"
    SECRET=$(cat /tmp/route-agent-secret.tmp)
    rm -f /tmp/route-agent-secret.tmp
  fi
fi

cat << EOT > "$AGENT_DIR/.env"
PORT=$PORT
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=$SECRET
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
CA_CERT_PATH=/etc/route-agent/certs/ca.crt
AGENT_CERT_PATH=/etc/route-agent/certs/agent.crt
AGENT_KEY_PATH=/etc/route-agent/certs/agent.key
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

if [ -n "$DOMAIN" ]; then
  echo "🎉 Route Agent initialized successfully on port $PORT (domain: $DOMAIN, decoy port: $DECOY_PORT)! Awaiting Orchestrator binary push..."
else
  echo "🎉 Route Agent initialized successfully on port $PORT! Awaiting Orchestrator binary push..."
fi