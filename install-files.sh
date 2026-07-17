#!/usr/bin/env bash
# Установка habits-files-agent на домашнюю машину для страницы «My Files».
# Агент держит исходящий WebSocket к бэкенду Habits (внешний IP и проброс
# портов не нужны) и даёт доступ к указанным папкам: только чтение (ro) или
# чтение и запись (rw). Токен выдаёт приложение: My Files → «＋ Добавить машину».
#
# Использование (от root):
#   ./install-files.sh <TOKEN> "<ПАПКИ>" [--url URL] [--user ИМЯ]
#
#   <ПАПКИ>   список «путь:режим» через ';', режим ro|rw (по умолчанию ro),
#             например: "/home/me/media:ro;/home/me/box:rw"
#   --url     endpoint бэкенда (по умолчанию прод Habits, wss://)
#   --user    от чьего имени запускать агент (он должен иметь доступ к папкам);
#             по умолчанию — пользователь, вызвавший sudo
set -euo pipefail

TOKEN="${1:-}"
ROOTS="${2:-}"
URL="wss://telegram.resager.ru/app/habits/api/v1/files/agent"
RUN_USER="${SUDO_USER:-root}"
REPO="resagera/habits-agent"

if [[ -z "$TOKEN" || "$TOKEN" == --* || -z "$ROOTS" || "$ROOTS" == --* ]]; then
    echo "Использование: $0 <TOKEN> \"/путь:ro;/путь2:rw\" [--url URL] [--user ИМЯ]"
    echo "Токен выдаёт приложение Habits: My Files → ＋ Добавить машину"
    exit 1
fi
shift 2

while [[ $# -gt 0 ]]; do
    case "$1" in
        --url)  URL="$2"; shift 2 ;;
        --user) RUN_USER="$2"; shift 2 ;;
        *) echo "неизвестный параметр: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Запустите от root: sudo ./install-files.sh ..."
    exit 1
fi

echo "==> 1/3 Бинарник"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -x "$SCRIPT_DIR/habits-files-agent" ]]; then
    echo "    использую готовый бинарник рядом со скриптом"
    install -m 755 "$SCRIPT_DIR/habits-files-agent" /usr/local/bin/habits-files-agent
elif command -v go >/dev/null 2>&1 && [[ -f "$SCRIPT_DIR/files-agent/main.go" ]]; then
    echo "    сборка из исходников (go build)"
    (cd "$SCRIPT_DIR/files-agent" && CGO_ENABLED=0 go build -ldflags '-s -w' -o /usr/local/bin/habits-files-agent .)
else
    echo "    скачиваю релиз с GitHub"
    ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; armv6l|armv7l) ARCH=arm ;; esac
    curl -fsSL "https://github.com/$REPO/releases/latest/download/habits-files-agent-linux-$ARCH" \
        -o /usr/local/bin/habits-files-agent
    chmod 755 /usr/local/bin/habits-files-agent
fi

echo "==> 2/3 Конфиг"
cat > /etc/habits-files-agent.env <<EOF
FILES_AGENT_TOKEN=$TOKEN
FILES_AGENT_URL=$URL
FILES_AGENT_ROOTS=$ROOTS
EOF
chmod 600 /etc/habits-files-agent.env

echo "==> 3/3 systemd (запуск от пользователя $RUN_USER)"
cat > /etc/systemd/system/habits-files-agent.service <<UNIT
[Unit]
Description=Habits files agent (My Files)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/habits-files-agent.env
ExecStart=/usr/local/bin/habits-files-agent
User=$RUN_USER
Restart=always
RestartSec=10
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now habits-files-agent
systemctl restart habits-files-agent
sleep 2
systemctl is-active habits-files-agent >/dev/null && echo "    сервис запущен"

echo
echo "Готово. Папки: $ROOTS"
echo "Машина появится в приложении Habits (My Files) в течение нескольких секунд."
