#!/usr/bin/env bash
# Установка habits-agent на ДОМАШНЮЮ машину (без внешнего IP) в push-режиме:
# агент сам шлёт метрики на бэкенд Habits раз в минуту, открытые порты и
# проброс не нужны. Токен выдаёт приложение: Servers → «＋ Добавить» →
# «🏠 Домашняя машина».
#
# Использование (от root):
#   ./install-home.sh <PUSH_TOKEN> [--url URL] [--port 9102] [--interval 60s]
#
#   --url       endpoint бэкенда (по умолчанию прод Habits)
#   --port      дополнительно отдавать GET /metrics на этом порту локально
#               (для отладки); не задан — агент порты не занимает
#   --interval  период отправки (минимум 10s, по умолчанию 60s)
set -euo pipefail

TOKEN="${1:-}"
URL="https://telegram.resager.ru/app/habits/api/v1/agent/push"
PORT=""
INTERVAL=""
REPO="resagera/habits-agent"

if [[ -z "$TOKEN" || "$TOKEN" == --* ]]; then
    echo "Использование: $0 <PUSH_TOKEN> [--url URL] [--port 9102] [--interval 60s]"
    echo "Токен выдаёт приложение Habits: Servers → ＋ Добавить → 🏠 Домашняя машина"
    exit 1
fi
shift

while [[ $# -gt 0 ]]; do
    case "$1" in
        --url)      URL="$2"; shift 2 ;;
        --port)     PORT="$2"; shift 2 ;;
        --interval) INTERVAL="$2"; shift 2 ;;
        *) echo "неизвестный параметр: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Запустите от root: sudo ./install-home.sh ..."
    exit 1
fi

echo "==> 1/3 Бинарник"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -x "$SCRIPT_DIR/habits-agent" ]]; then
    echo "    использую готовый бинарник рядом со скриптом"
    install -m 755 "$SCRIPT_DIR/habits-agent" /usr/local/bin/habits-agent
elif command -v go >/dev/null 2>&1 && [[ -f "$SCRIPT_DIR/main.go" ]]; then
    echo "    сборка из исходников (go build)"
    (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -ldflags '-s -w' -o /usr/local/bin/habits-agent .)
else
    echo "    скачиваю релиз с GitHub"
    ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
    curl -fsSL "https://github.com/$REPO/releases/latest/download/habits-agent-linux-$ARCH" \
        -o /usr/local/bin/habits-agent
    chmod 755 /usr/local/bin/habits-agent
fi

echo "==> 2/3 Конфиг"
cat > /etc/habits-agent.env <<EOF
AGENT_TOKEN=$TOKEN
AGENT_PUSH_URL=$URL
${PORT:+AGENT_ADDR=:$PORT}
${INTERVAL:+AGENT_PUSH_INTERVAL=$INTERVAL}
EOF
chmod 600 /etc/habits-agent.env

echo "==> 3/3 systemd"
cat > /etc/systemd/system/habits-agent.service <<'UNIT'
[Unit]
Description=Habits monitoring agent (push mode)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/habits-agent.env
ExecStart=/usr/local/bin/habits-agent
Restart=always
RestartSec=10
User=nobody
ProtectSystem=strict
ProtectHome=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now habits-agent
systemctl restart habits-agent
sleep 2
systemctl is-active habits-agent >/dev/null && echo "    сервис запущен"

echo
echo "Готово: агент шлёт отчёты раз в ${INTERVAL:-60s} на $URL"
echo "Карточка машины в приложении Habits наполнится в течение минуты."
if [[ -n "$PORT" ]]; then
    echo "Локальные метрики: curl -H 'Authorization: Bearer $TOKEN' http://localhost:$PORT/metrics"
fi
