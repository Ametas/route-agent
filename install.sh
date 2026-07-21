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
  exit 1
fi

# 1. Установка системных зависимостей
echo "📦 Installing system packages..."
apt-get update
apt-get install -y iptables iproute2 ufw git curl unzip debian-keyring debian-archive-keyring apt-transport-https ca-certificates gnupg caddy || true

# 2. Настройка UFW
ufw allow 22/tcp || true
ufw allow 443 || true
ufw allow "$PORT" || true
ufw --force enable || true

cat > /etc/sudoers.d/route-agent-ufw <<'EOF'
ALL ALL=(ALL) NOPASSWD: /usr/sbin/ufw
EOF
chmod 0440 /etc/sudoers.d/route-agent-ufw

# 3. Базовая заготовка для службы sing-box (без бинарника, бинарник зальет оркестратор)
mkdir -p /etc/sing-box /var/www/decoy
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
ExecReload=/bin/sh -c "/usr/local/bin/sing-box check -c /etc/sing-box/config.json && /bin/kill -HUP \$MAINPID"

[Install]
WantedBy=multi-user.target
EOT

systemctl daemon-reload

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

cat << EOT > "$AGENT_DIR/.env"
PORT=$PORT
HOST=0.0.0.0
EGRESS_CONTROL_SECRET=$SECRET
SINGBOX_CONFIG_PATH=/etc/sing-box/config.json
RELOAD_COMMAND=systemctl reload sing-box
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

echo "🎉 Route Agent initialized successfully on port $PORT! Awaiting Orchestrator binary push..."