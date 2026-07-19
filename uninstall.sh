#!/bin/bash
set -e

echo "🗑️ Starting Route Agent and Infrastructure Uninstallation..."

# Проверяем, запущен ли скрипт от root
if [ "$EUID" -ne 0 ]; then
  echo "❌ Please run as root (sudo)"
  exit 1
fi

# 1. Остановка и отключение служб из автозагрузки
echo "🛑 Stopping and disabling systemd services..."
systemctl stop route-agent || true
systemctl disable route-agent || true

systemctl stop olcrtc || true
systemctl disable olcrtc || true

systemctl stop caddy || true
systemctl disable caddy || true

# 2. Удаление конфигурационных файлов systemd
echo "📂 Purging systemd unit configurations..."
rm -f /etc/systemd/system/route-agent.service
rm -f /etc/systemd/system/olcrtc.service

# Перезагружаем менеджер systemd, чтобы применить удаление юнитов
systemctl daemon-reload

# 3. Полная очистка рабочих папок и бинарников
echo "🧹 Erasing binaries, repositories, and static decoy paths..."
rm -rf /opt/route-agent
rm -f /usr/local/bin/olcrtc
rm -rf /var/www/decoy

# 4. Удаление Caddy и его ключей (если Caddy был установлен)
if command -v caddy &> /dev/null; then
  echo "📦 Purging Caddy Web Server package..."
  apt-get purge -y caddy || true
  rm -f /etc/apt/sources.list.d/caddy-stable.list
  rm -f /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  apt-get autoremove -y || true
fi

echo "---"
echo "🎉 UninstallationPass complete! Your VPS is completely clean of Route Agent, Caddy Decoy, and WebRTC layers."