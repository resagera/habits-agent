# habits-agent

Крошечный агент мониторинга Linux-сервера для страницы **Servers**
приложения [Habits](https://github.com/resagera/habits-tg-webapp)
(Telegram Mini App). Один статический бинарник, только stdlib и `/proc`,
без зависимостей.

Отдаёт `GET /metrics` (JSON):

- ОС (`PRETTY_NAME`), ядро, архитектура, hostname
- внешний IP (кэш 1 час)
- uptime
- CPU: загрузка % (сэмпл `/proc/stat`), число ядер, load average
- RAM: всего / занято / доступно
- диски: список смонтированных ФС, всего / свободно по каждому

Защита — токен: `Authorization: Bearer <токен>`. Без верного токена — 401.

```json
{
  "hostname": "vps1", "os": "Ubuntu 24.04 LTS", "kernel": "6.8.0",
  "arch": "amd64", "external_ip": "1.2.3.4", "uptime_sec": 123456,
  "cpu_pct": 3.1, "cpu_cores": 2, "load1": 0.12,
  "ram": {"total": 2000000000, "used": 900000000, "available": 1100000000},
  "disks": [{"mount": "/", "device": "/dev/vda1", "total": 42000000000, "free": 17000000000}]
}
```

## Установка

Одной командой (от root):

```bash
curl -fsSL https://raw.githubusercontent.com/resagera/habits-agent/main/install.sh | bash
```

Или из клона репозитория:

```bash
git clone https://github.com/resagera/habits-agent.git
cd habits-agent
sudo ./install.sh
```

Скрипт: ставит бинарник в `/usr/local/bin/habits-agent` (готовый рядом со
скриптом → сборка `go build`, если есть Go → скачивание релиза с GitHub),
генерирует токен в `/etc/habits-agent.env` (при повторной установке токен
сохраняется), создаёт systemd-сервис `habits-agent` и запускает его.

### Параметры

```bash
sudo ./install.sh [--domain agent.example.com] [--port 9101] [--token XXX] [--open all|docker|none]
```

| Параметр | Описание |
|---|---|
| `--domain` | доступный домен этого сервера: скрипт **добавит server-блок в конфиг nginx** (`/etc/nginx/conf.d/habits-agent.conf`, проверка `nginx -t`, graceful reload) — метрики станут доступны по `http://<домен>/metrics`. Если nginx не установлен, шаг пропускается. Для HTTPS после установки: `certbot --nginx -d <домен>` |
| `--port` | порт агента (по умолчанию `9101`) |
| `--token` | свой токен; не задан — генерируется (или сохраняется прежний) |
| `--open` | правило ufw: `all` — порт открыт отовсюду, `docker` — только докер-подсетям `172.16.0.0/12` (по умолчанию; удобно, когда Habits-бэкенд крутится в docker на этой же машине), `none` — firewall не трогать |

В конце скрипт печатает **адрес и токен** — их нужно ввести в приложении
Habits: страница **Servers → «Добавить сервер»**. Бэкенд Habits опрашивает
адрес раз в минуту и рисует графики CPU/RAM за 24 часа, диски и uptime.

### Примеры

```bash
# агент за nginx на своём домене, порт наружу не открывать
sudo ./install.sh --domain agent.example.com --open none

# просто по порту, доступ отовсюду (токен защищает данные)
sudo ./install.sh --open all
```

## Обновление

Повторный запуск `install.sh` обновляет бинарник и unit, сохраняя токен.

## Удаление

```bash
sudo systemctl disable --now habits-agent
sudo rm /usr/local/bin/habits-agent /etc/habits-agent.env /etc/systemd/system/habits-agent.service
sudo rm -f /etc/nginx/conf.d/habits-agent.conf && sudo systemctl reload nginx
```

## Сборка вручную

```bash
CGO_ENABLED=0 go build -ldflags '-s -w' -o habits-agent .
```
