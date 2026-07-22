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

# 1. Остановка и отключение всех управляемых служб из автозагрузки
echo "🛑 Stopping and disabling systemd services..."
systemctl stop route-agent || true
systemctl disable route-agent || true

systemctl stop sing-box || true
systemctl disable sing-box || true

systemctl stop olcrtc || true
systemctl disable olcrtc || true

systemctl stop caddy || true
systemctl disable caddy || true

# 2. Очистка правил фаервола UFW и файла sudoers
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

  echo "🔄 Reloading UFW rules..."
  ufw reload || true
else
  echo "⏭️ UFW is not active or installed. Skipping firewall rules cleanup..."
fi

rm -f /etc/sudoers.d/route-agent-ufw

# 3. Удаление конфигурационных файлов systemd
echo "📂 Purging systemd unit configurations..."
rm -f /etc/systemd/system/route-agent.service
rm -f /etc/systemd/system/sing-box.service
rm -f /etc/systemd/system/olcrtc.service

# Перезагружаем менеджер systemd, чтобы применить удаление юнитов
systemctl daemon-reload

# 4. Полная очистка рабочих папок, бинарников и конфигураций
echo "🧹 Erasing binaries, repositories, config files, and static decoy paths..."
rm -rf /opt/route-agent
rm -rf /etc/sing-box
rm -rf /etc/caddy
rm -rf /var/www/decoy
rm -f /usr/local/bin/sing-box
rm -f /usr/local/bin/olcrtc
rm -f /usr/local/bin/olcrtc-manager
rm -f /tmp/sing-box.download /tmp/olcrtc.download /tmp/olcrtc-manager.download

# 5. Удаление Caddy и его ключей (если Caddy был установлен)
if command -v caddy &> /dev/null; then
  echo "📦 Purging Caddy Web Server package..."
  apt-get purge -y caddy || true
  rm -f /etc/apt/sources.list.d/caddy-stable.list
  rm -f /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  apt-get autoremove -y || true
fi

echo "---"
echo "🎉 Uninstallation complete! Your VPS is clean of Route Agent, sing-box, Caddy, and Olcrtc WebRTC components."