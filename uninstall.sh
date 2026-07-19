#!/bin/bash
set -e

echo "🗑️ Starting Route Agent and Infrastructure Uninstallation..."

# Проверяем, запущен ли скрипт от root
if [ "$EUID" -ne 0 ]; then
  echo "❌ Please run as root (sudo)"
  exit 1
fi

# Дефолтные порты для закрытия (если не переданы другие)
PORT="8081"
OLCRTC_PORT="8888"

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --port) PORT="$2"; shift ;;
        --olcrtc-port) OLCRTC_PORT="$2"; shift ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

# 1. Остановка и отключение служб из автозагрузки
echo "🛑 Stopping and disabling systemd services..."
systemctl stop route-agent || true
systemctl disable route-agent || true

systemctl stop olcrtc || true
systemctl disable olcrtc || true

systemctl stop caddy || true
systemctl disable caddy || true

# 2. Очистка правил фаервола UFW (Пакет UFW не удаляется)
if command -v ufw &> /dev/null; then
  echo "🛡️ Cleaning up UFW firewall rules..."
  
  echo "🔒 Closing gRPC Agent port $PORT..."
  ufw delete allow "$PORT" || true
  ufw delete allow "$PORT"/tcp || true
  ufw delete allow "$PORT"/udp || true

  echo "🔒 Closing WebRTC Olcrtc port $OLCRTC_PORT..."
  ufw delete allow "$OLCRTC_PORT" || true
  ufw delete allow "$OLCRTC_PORT"/tcp || true
  ufw delete allow "$OLCRTC_PORT"/udp || true

  echo "🔄 Reloading UFW rules (Ports 22 and 443 are left untouched)..."
  ufw reload
else
  echo "⏭️ UFW is not active or installed. Skipping firewall rules cleanup..."
fi

# 3. Удаление конфигурационных файлов systemd
echo "📂 Purging systemd unit configurations..."
rm -f /etc/systemd/system/route-agent.service
rm -f /etc/systemd/system/olcrtc.service

# Перезагружаем менеджер systemd, чтобы применить удаление юнитов
systemctl daemon-reload

# 4. Полная очистка рабочих папок и бинарников
echo "🧹 Erasing binaries, repositories, and static decoy paths..."
rm -rf /opt/route-agent
rm -f /usr/local/bin/olcrtc
rm -rf /var/www/decoy

# 5. Удаление Caddy и его ключей (если Caddy был установлен)
if command -v caddy &> /dev/null; then
  echo "📦 Purging Caddy Web Server package..."
  apt-get purge -y caddy || true
  rm -f /etc/apt/sources.list.d/caddy-stable.list
  rm -f /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  apt-get autoremove -y || true
fi

echo "---"
echo "🎉 Uninstallation complete! Your VPS is clean of Route Agent, Caddy Decoy, and WebRTC layers."
echo "🛡️ UFW ports $PORT and $OLCRTC_PORT are successfully closed. Infrastructure ports 22 and 443 remain open."