#!/usr/bin/env bash
# Установка habits-term-agent на домашнюю машину для страницы Terminal.
# Агент держит исходящий WebSocket к бэкенду Habits (внешний IP и проброс
# портов не нужны) и по запросу открывает PTY-сессии — веб-консоль к машине.
#
# ВНИМАНИЕ: даёт полный доступ к shell под пользователем, от которого запущен.
# Ставьте только на свои машины, храните токен в секрете.
#
# Использование (от root):
#   ./install-term.sh <TOKEN> [--url URL] [--user ИМЯ] [--dir КАТАЛОГ]
#
#   --url   endpoint бэкенда (по умолчанию прод Habits, wss://)
#   --user  от чьего имени запускать (его shell и права); по умолчанию — тот,
#           кто вызвал sudo
#   --dir   стартовый каталог сессий (по умолчанию домашний каталог)
set -euo pipefail

TOKEN="${1:-}"
URL="wss://telegram.resager.ru/app/habits/api/v1/terminal/agent"
RUN_USER="${SUDO_USER:-root}"
DIR=""
REPO="resagera/habits-agent"

if [[ -z "$TOKEN" || "$TOKEN" == --* ]]; then
    echo "Использование: $0 <TOKEN> [--url URL] [--user ИМЯ] [--dir КАТАЛОГ]"
    echo "Токен выдаёт приложение Habits: Terminal → ＋ Добавить машину"
    exit 1
fi
shift

while [[ $# -gt 0 ]]; do
    case "$1" in
        --url)  URL="$2"; shift 2 ;;
        --user) RUN_USER="$2"; shift 2 ;;
        --dir)  DIR="$2"; shift 2 ;;
        *) echo "неизвестный параметр: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Запустите от root: sudo ./install-term.sh ..."
    exit 1
fi

echo "==> 1/3 Бинарник"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -x "$SCRIPT_DIR/habits-term-agent" ]]; then
    echo "    использую готовый бинарник рядом со скриптом"
    install -m 755 "$SCRIPT_DIR/habits-term-agent" /usr/local/bin/habits-term-agent
elif command -v go >/dev/null 2>&1 && [[ -f "$SCRIPT_DIR/term-agent/main.go" ]]; then
    echo "    сборка из исходников (go build)"
    (cd "$SCRIPT_DIR/term-agent" && CGO_ENABLED=0 go build -ldflags '-s -w' -o /usr/local/bin/habits-term-agent .)
else
    echo "    скачиваю релиз с GitHub"
    ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; armv6l|armv7l) ARCH=arm ;; esac
    curl -fsSL "https://github.com/$REPO/releases/latest/download/habits-term-agent-linux-$ARCH" \
        -o /usr/local/bin/habits-term-agent
    chmod 755 /usr/local/bin/habits-term-agent
fi

echo "==> 2/3 Конфиг"
cat > /etc/habits-term-agent.env <<EOF
TERM_AGENT_TOKEN=$TOKEN
TERM_AGENT_URL=$URL
${DIR:+TERM_AGENT_DIR=$DIR}
EOF
chmod 600 /etc/habits-term-agent.env

echo "==> 3/3 systemd (запуск от пользователя $RUN_USER)"
cat > /etc/systemd/system/habits-term-agent.service <<UNIT
[Unit]
Description=Habits terminal agent (Terminal page)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/habits-term-agent.env
ExecStart=/usr/local/bin/habits-term-agent
User=$RUN_USER
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now habits-term-agent
systemctl restart habits-term-agent
sleep 2
systemctl is-active habits-term-agent >/dev/null && echo "    сервис запущен"

echo
echo "Готово. Консоль от пользователя $RUN_USER."
echo "Откройте приложение Habits → Terminal → «Открыть консоль»."
