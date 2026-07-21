#!/usr/bin/env bash
# Установка habits-automation-agent на домашнюю машину для страницы
# «Автоматизация». jur.am стоит за Cloudflare и блокирует IP сервера, поэтому
# HTTP-запросы к сайту идут через агента на вашей машине (резидентный IP).
# Агент держит исходящий WebSocket к бэкенду Habits (внешний IP и проброс
# портов не нужны) и выполняет одиночные запросы только к jur.am. Токен выдаёт
# приложение: Автоматизация → «Домашний агент».
#
# Использование (от root):
#   ./install-automation.sh <TOKEN> [--url URL] [--user ИМЯ]
set -euo pipefail

TOKEN="${1:-}"
URL="wss://telegram.resager.ru/app/habits/api/v1/automation/agent"
RUN_USER="${SUDO_USER:-root}"
REPO="resagera/habits-agent"

if [[ -z "$TOKEN" || "$TOKEN" == --* ]]; then
    echo "Использование: $0 <TOKEN> [--url URL] [--user ИМЯ]"
    echo "Токен выдаёт приложение Habits: Автоматизация → Домашний агент"
    exit 1
fi
shift

while [[ $# -gt 0 ]]; do
    case "$1" in
        --url)  URL="$2"; shift 2 ;;
        --user) RUN_USER="$2"; shift 2 ;;
        *) echo "неизвестный параметр: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Запустите от root: sudo ./install-automation.sh <TOKEN>"
    exit 1
fi

echo "==> 1/3 Бинарник"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -x "$SCRIPT_DIR/habits-automation-agent" ]]; then
    echo "    использую готовый бинарник рядом со скриптом"
    install -m 755 "$SCRIPT_DIR/habits-automation-agent" /usr/local/bin/habits-automation-agent
elif command -v go >/dev/null 2>&1 && [[ -f "$SCRIPT_DIR/automation-agent/main.go" ]]; then
    echo "    сборка из исходников (go build)"
    (cd "$SCRIPT_DIR/automation-agent" && CGO_ENABLED=0 go build -ldflags '-s -w' -o /usr/local/bin/habits-automation-agent .)
else
    echo "    скачиваю релиз с GitHub"
    ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; armv6l|armv7l) ARCH=arm ;; esac
    curl -fsSL "https://github.com/$REPO/releases/latest/download/habits-automation-agent-linux-$ARCH" \
        -o /usr/local/bin/habits-automation-agent
    chmod 755 /usr/local/bin/habits-automation-agent
fi

echo "==> 2/3 Конфиг"
cat > /etc/habits-automation-agent.env <<EOF
AUTOMATION_AGENT_TOKEN=$TOKEN
AUTOMATION_AGENT_URL=$URL
EOF
chmod 600 /etc/habits-automation-agent.env

echo "==> 3/3 systemd (запуск от пользователя $RUN_USER)"
cat > /etc/systemd/system/habits-automation-agent.service <<UNIT
[Unit]
Description=Habits automation agent (jur.am egress)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/habits-automation-agent.env
ExecStart=/usr/local/bin/habits-automation-agent
User=$RUN_USER
Restart=always
RestartSec=10
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now habits-automation-agent
systemctl restart habits-automation-agent
sleep 2
systemctl is-active habits-automation-agent >/dev/null && echo "    сервис запущен"

echo
echo "Готово. Агент подключится к $URL"
echo "Статус «в сети» появится на странице Автоматизация в течение нескольких секунд."
