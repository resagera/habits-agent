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

## Домашние машины без внешнего IP (push-режим)

Если до машины нельзя достучаться снаружи (домашний ПК за NAT), агент
работает наоборот: **сам шлёт** тот же JSON-отчёт раз в минуту на бэкенд
Habits (`POST /api/v1/agent/push`, авторизация Bearer push-токеном).
Открытые порты и проброс не нужны.

1. В приложении Habits: **Servers → «＋ Добавить» → «🏠 Домашняя машина»** —
   получите push-токен и команду установки.
2. На машине (от root):

```bash
curl -fsSL https://raw.githubusercontent.com/resagera/habits-agent/main/install-home.sh | sudo bash -s -- <PUSH_TOKEN>
```

Или из клона: `sudo ./install-home.sh <PUSH_TOKEN>`.

Параметры: `--url` (endpoint бэкенда, по умолчанию прод Habits), `--port`
(дополнительно отдавать локальный `GET /metrics`, по умолчанию порты не
занимаются), `--interval` (период отправки, по умолчанию `60s`).

Переменные окружения агента в push-режиме: `AGENT_PUSH_URL` (включает
режим), `AGENT_TOKEN` (push-токен), `AGENT_PUSH_INTERVAL`, `AGENT_ADDR`
(если задан — параллельно слушает и `GET /metrics`).

Если машина замолкает дольше 3 минут, владельцу приходит сообщение бота
«🔴 не выходит на связь», при восстановлении — «🟢 снова на связи»
(работает и для обычных pull-серверов).

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

---

## habits-files-agent — страница «My Files»

Отдельный агент для доступа к файлам домашней машины из приложения. Держит
исходящий WebSocket к бэкенду (внешний IP не нужен) и работает только с
указанными папками: `ro` — только чтение, `rw` — чтение и запись. Выход за
пределы папок (в т.ч. через `..` и симлинки) запрещён. Исходники — в
каталоге `files-agent/`.

### Установка

Токен и папки выдаёт приложение: **My Files → ＋ Добавить машину**.

```bash
curl -fsSL https://raw.githubusercontent.com/resagera/habits-agent/main/install-files.sh \
  | sudo bash -s -- <ТОКЕН> "/home/me/media:ro;/home/me/box:rw"
```

Агент по умолчанию запускается от имени пользователя, вызвавшего `sudo`
(он должен иметь доступ к папкам) — переопределяется флагом `--user`.

### Переменные окружения

- `FILES_AGENT_URL` — endpoint бэкенда (`wss://…/api/v1/files/agent`)
- `FILES_AGENT_TOKEN` — токен машины
- `FILES_AGENT_ROOTS` — папки `путь:режим` через `;` (режим `ro`|`rw`)

### Удаление

```bash
sudo systemctl disable --now habits-files-agent
sudo rm /usr/local/bin/habits-files-agent /etc/habits-files-agent.env \
  /etc/systemd/system/habits-files-agent.service
```

### Сборка вручную

```bash
cd files-agent && CGO_ENABLED=0 go build -ldflags '-s -w' -o habits-files-agent .
```
