#!/usr/bin/env bash
# Установка habits-agent — агента мониторинга для страницы Servers
# приложения Habits (https://github.com/resagera/habits-tg-webapp).
#
# Использование (от root):
#   ./install.sh [--domain agent.example.com] [--port 9101] [--token XXX] [--open all|docker|none]
#
#   --domain  доступный домен на этом сервере: скрипт добавит server-блок
#             в конфиг nginx (если nginx установлен) и метрики будут
#             доступны по http://<домен>/metrics
#   --port    порт агента (по умолчанию 9101)
#   --token   токен авторизации; не задан — генерируется (или берётся прежний)
#   --open    правило ufw для порта: all — отовсюду, docker — только
#             docker-подсетям (по умолчанию), none — не трогать firewall
set -euo pipefail

DOMAIN=""
PORT=9101
TOKEN=""
OPEN="docker"
REPO="resagera/habits-agent"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --domain) DOMAIN="$2"; shift 2 ;;
        --port)   PORT="$2"; shift 2 ;;
        --token)  TOKEN="$2"; shift 2 ;;
        --open)   OPEN="$2"; shift 2 ;;
        *) echo "неизвестный параметр: $1"; exit 1 ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Запустите от root: sudo ./install.sh ..."
    exit 1
fi

echo "==> 1/5 Бинарник"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -x "$SCRIPT_DIR/habits-agent" ]]; then
    echo "    использую готовый бинарник рядом со скриптом"
    install -m 755 "$SCRIPT_DIR/habits-agent" /usr/local/bin/habits-agent
elif command -v go >/dev/null 2>&1 && [[ -f "$SCRIPT_DIR/main.go" ]]; then
    echo "    сборка из исходников (go build)"
    (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -ldflags '-s -w' -o /usr/local/bin/habits-agent .)
else
    echo "    скачиваю релиз с GitHub"
    ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; armv6l|armv7l) ARCH=arm ;; esac
    curl -fsSL "https://github.com/$REPO/releases/latest/download/habits-agent-linux-$ARCH" \
        -o /usr/local/bin/habits-agent
    chmod 755 /usr/local/bin/habits-agent
fi

echo "==> 2/5 Токен и конфиг"
if [[ -z "$TOKEN" && -f /etc/habits-agent.env ]]; then
    TOKEN=$(grep -oP 'AGENT_TOKEN=\K.*' /etc/habits-agent.env || true)
    [[ -n "$TOKEN" ]] && echo "    оставляю прежний токен из /etc/habits-agent.env"
fi
if [[ -z "$TOKEN" ]]; then
    TOKEN=$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)
    echo "    сгенерирован новый токен"
fi
cat > /etc/habits-agent.env <<EOF
AGENT_TOKEN=$TOKEN
AGENT_ADDR=:$PORT
EOF
chmod 600 /etc/habits-agent.env

echo "==> 3/5 systemd"
cat > /etc/systemd/system/habits-agent.service <<'UNIT'
[Unit]
Description=Habits monitoring agent (/metrics)
After=network-online.target

[Service]
EnvironmentFile=/etc/habits-agent.env
ExecStart=/usr/local/bin/habits-agent
Restart=always
RestartSec=5
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
sleep 1
systemctl is-active habits-agent >/dev/null && echo "    сервис запущен"

echo "==> 4/5 Firewall (ufw)"
if command -v ufw >/dev/null 2>&1 && ufw status | grep -q 'Status: active'; then
    case "$OPEN" in
        all)    ufw allow "$PORT"/tcp comment 'habits-agent' >/dev/null; echo "    порт $PORT открыт отовсюду" ;;
        docker) ufw allow from 172.16.0.0/12 to any port "$PORT" proto tcp comment 'habits-agent (docker only)' >/dev/null
                echo "    порт $PORT открыт только docker-подсетям" ;;
        none)   echo "    firewall не трогаю (--open none)" ;;
    esac
else
    echo "    ufw не активен — пропускаю"
fi

echo "==> 5/5 nginx"
URL="http://<ip-сервера>:$PORT/metrics"
if [[ -n "$DOMAIN" ]]; then
    if command -v nginx >/dev/null 2>&1; then
        CONF=/etc/nginx/conf.d/habits-agent.conf
        [[ -d /etc/nginx/conf.d ]] || { mkdir -p /etc/nginx/conf.d; }
        cat > "$CONF" <<NGINX
# habits-agent: метрики сервера для приложения Habits (добавлено install.sh)
server {
    listen 80;
    server_name $DOMAIN;

    location /metrics {
        proxy_pass http://127.0.0.1:$PORT/metrics;
        proxy_set_header Host \$host;
    }
}
NGINX
        if nginx -t >/dev/null 2>&1; then
            systemctl reload nginx 2>/dev/null || nginx -s reload
            echo "    добавлен $CONF, nginx перезагружен"
            URL="http://$DOMAIN/metrics"
            echo "    для HTTPS выполните: certbot --nginx -d $DOMAIN"
        else
            rm -f "$CONF"
            echo "    ОШИБКА: конфиг не прошёл nginx -t — откатил, проверьте вручную"
        fi
    else
        echo "    nginx не установлен — домен пропущен, агент доступен по порту $PORT"
    fi
else
    echo "    домен не задан (--domain) — пропускаю"
fi

echo
echo "Готово! Проверка:"
echo "  curl -H 'Authorization: Bearer $TOKEN' http://localhost:$PORT/metrics"
echo
echo "Добавьте сервер на странице Servers приложения Habits:"
echo "  Адрес: $URL"
echo "  Токен: $TOKEN"
