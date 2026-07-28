#!/usr/bin/env bash
# Установка habits-ai-agent на домашнюю машину для страницы AI.
# Агент держит исходящий WebSocket к бэкенду Habits и запускает Claude Code
# (print mode) по задачам из приложения — только в разрешённых папках.
#
# ВНИМАНИЕ: по умолчанию Claude Code выполняется с полным bypass прав
# (--dangerously-skip-permissions) в указанных папках. Ставьте только на свои
# машины, указывайте только свои папки, храните токен в секрете.
# Claude Code должен быть установлен и авторизован у пользователя запуска
# (команда claude → вход в аккаунт).
#
# Использование (от root):
#   ./install-ai.sh <TOKEN> --dirs "/path/one;/path/two" [--url URL] [--user ИМЯ] [--no-bypass]
#
#   --dirs      разрешённые папки через ';' (обязательно)
#   --url       endpoint бэкенда (по умолчанию прод Habits, wss://)
#   --user      от чьего имени запускать (его claude и права); по умолчанию —
#               тот, кто вызвал sudo
#   --no-bypass запускать Claude Code без --dangerously-skip-permissions
set -euo pipefail

TOKEN="${1:-}"
URL="wss://telegram.resager.ru/app/habits/api/v1/ai/agent"
RUN_USER="${SUDO_USER:-root}"
DIRS=""
BYPASS=1
REPO="resagera/habits-agent"

if [[ -z "$TOKEN" || "$TOKEN" == --* ]]; then
    echo "Использование: $0 <TOKEN> --dirs \"/path/one;/path/two\" [--url URL] [--user ИМЯ] [--no-bypass]"
    echo "Токен выдаёт приложение Habits: AI → ＋ Машина"
    exit 1
fi
shift

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dirs) DIRS="$2"; shift 2 ;;
        --url)  URL="$2"; shift 2 ;;
        --user) RUN_USER="$2"; shift 2 ;;
        --no-bypass) BYPASS=0; shift ;;
        *) echo "неизвестный параметр: $1"; exit 1 ;;
    esac
done

if [[ -z "$DIRS" ]]; then
    echo "--dirs обязателен: разрешённые папки через ';'"
    exit 1
fi

if [[ $EUID -ne 0 ]]; then
    echo "Запустите от root: sudo ./install-ai.sh ..."
    exit 1
fi

echo "==> 1/3 Бинарник"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -x "$SCRIPT_DIR/habits-ai-agent" ]]; then
    echo "    использую готовый бинарник рядом со скриптом"
    install -m 755 "$SCRIPT_DIR/habits-ai-agent" /usr/local/bin/habits-ai-agent
elif command -v go >/dev/null 2>&1 && [[ -f "$SCRIPT_DIR/ai-agent/main.go" ]]; then
    echo "    сборка из исходников (go build)"
    (cd "$SCRIPT_DIR/ai-agent" && CGO_ENABLED=0 go build -ldflags '-s -w' -o /usr/local/bin/habits-ai-agent .)
else
    echo "    скачиваю релиз с GitHub"
    ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; armv6l|armv7l) ARCH=arm ;; esac
    curl -fsSL "https://github.com/$REPO/releases/latest/download/habits-ai-agent-linux-$ARCH" \
        -o /usr/local/bin/habits-ai-agent
    chmod 755 /usr/local/bin/habits-ai-agent
fi

echo "==> 2/3 Конфиг"
cat > /etc/habits-ai-agent.env <<EOF
AI_AGENT_TOKEN=$TOKEN
AI_AGENT_URL=$URL
AI_AGENT_DIRS=$DIRS
AI_AGENT_BYPASS=$BYPASS
EOF
chmod 600 /etc/habits-ai-agent.env

echo "==> 3/3 systemd (запуск от пользователя $RUN_USER)"
cat > /etc/systemd/system/habits-ai-agent.service <<UNIT
[Unit]
Description=Habits AI agent (Claude Code runner)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/habits-ai-agent.env
ExecStart=/usr/local/bin/habits-ai-agent
User=$RUN_USER
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now habits-ai-agent
systemctl restart habits-ai-agent
sleep 2
systemctl is-active habits-ai-agent >/dev/null && echo "    сервис запущен"

echo
echo "Готово. Задачи выполняются от пользователя $RUN_USER в папках: $DIRS"
echo "Проверьте, что Claude Code авторизован: sudo -u $RUN_USER claude --version"
echo "Откройте приложение Habits → AI → Проверить."
