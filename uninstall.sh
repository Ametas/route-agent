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

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --port) PORT="$2"; shift ;;
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

# Тыловой sing-box (WARP-выход). Юнит создаётся АГЕНТОМ в рантайме по кнопке, а не этим
# скриптом, поэтому здесь он только снимается — иначе после удаления агента на машине остался
# бы включённый юнит, который systemd продолжал бы перезапускать.
systemctl stop route-rear-singbox || true
systemctl disable route-rear-singbox || true

# olcrtc-agent-srv (план `olcrtc-redesign.md`) — one templated unit INSTANCE per user, not a single
# fixed service name; stop/disable every currently-running instance before removing the template
# unit file itself below. No UFW port to close here — the new design never opens one (control is
# entirely gRPC + local systemd, no admin HTTP surface, unlike the old olcrtc-manager it replaces).
for unit in $(systemctl list-units --type=service --all --no-legend 'olcrtc-agent-srv@*' 2>/dev/null | awk '{print $1}'); do
  systemctl stop "$unit" || true
  systemctl disable "$unit" || true
done

systemctl stop caddy || true
systemctl disable caddy || true

systemctl stop awg-quick@awg0 || true
systemctl disable awg-quick@awg0 || true

# 2. Очистка правил фаервола UFW и файла sudoers
if command -v ufw &> /dev/null; then
  echo "🛡️ Cleaning up UFW firewall rules..."
  
  echo "🔒 Closing 443 tcp/udp..."
  ufw delete allow 443/tcp || true
  ufw delete allow 443/udp || true

  echo "🔒 Closing gRPC Agent port $PORT..."
  ufw delete allow "$PORT" || true
  ufw delete allow "$PORT"/tcp || true
  ufw delete allow "$PORT"/udp || true

  # No olcrtc port to close — the new olcrtc-agent-srv design (план `olcrtc-redesign.md`) never
  # opens a public port at all, unlike the old olcrtc-manager it replaces.

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
rm -f /etc/systemd/system/olcrtc-agent-srv@.service
rm -f /etc/systemd/system/route-rear-singbox.service
# В конфиге тыла лежат ПРИВАТНЫЕ КЛЮЧИ WARP — оставлять его на снимаемой ноде нельзя.
# Второй путь — старое место (до 2026-09-04), откуда конфиг переехал: на нодах, снимаемых без
# промежуточного обновления агента, он всё ещё лежит там. Каталог /etc/route-agent целиком удаляется
# ниже, но явная строка нужна на случай, если тот блок когда-нибудь сузят.
rm -f /etc/route-agent/rear.json /etc/sing-box/rear.json

# Профиль сетевых буферов ядра, который агент кладёт при каждом старте (utils/kernelTuning.ts).
# Снимается вместе с агентом: оставленный файл продолжал бы поднимать буферы на машине, где
# route-agent больше нет, — тихая настройка без владельца. Значения вернутся к умолчаниям ядра
# после перезагрузки; сбрасывать их здесь через sysctl -w незачем, машина всё равно снимается.
rm -f /etc/sysctl.d/99-route-agent.conf

# Перезагружаем менеджер systemd, чтобы применить удаление юнитов
systemctl daemon-reload

# 4. Полная очистка рабочих папок, бинарников и конфигураций
echo "🧹 Erasing binaries, repositories, config files, and static decoy paths..."
rm -rf /opt/route-agent
rm -rf /etc/route-agent
rm -rf /etc/sing-box
rm -rf /etc/caddy
rm -rf /etc/amnezia/amneziawg
rm -rf /etc/olcrtc-agent-srv
rm -rf /var/www/decoy
rm -f /usr/local/bin/sing-box
rm -f /usr/local/bin/olcrtc-agent-srv
rm -f /tmp/sing-box.download

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