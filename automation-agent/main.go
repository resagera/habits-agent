// habits-automation-agent — тонкий агент сетевого выхода для страницы
// «Автоматизация». jur.am стоит за Cloudflare и блокирует датацентр-IP
// прод-сервера; агент запускается на домашней машине (резидентный IP) и
// держит исходящий WebSocket к проду. Прод туннелирует через него одиночные
// HTTP-запросы к jur.am, вся логика заказа остаётся на сервере. Агент только
// выполняет запросы к белому списку хостов и возвращает ответ как есть
// (без своих cookie и без следования редиректам — этим управляет сервер).
//
// Переменные окружения:
//   AUTOMATION_AGENT_URL   — wss://telegram.resager.ru/app/habits/api/v1/automation/agent
//   AUTOMATION_AGENT_TOKEN — токен из карточки на странице «Автоматизация»
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// allowedHosts — белый список: агент не должен быть открытым прокси.
var allowedHosts = map[string]bool{
	"jur.am":     true,
	"www.jur.am": true,
}

const defaultURL = "wss://telegram.resager.ru/app/habits/api/v1/automation/agent"

type reqFrame struct {
	ID      uint64              `json:"id"`
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body,omitempty"`
}

type respFrame struct {
	ID      uint64              `json:"id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body,omitempty"`
	Error   string              `json:"error,omitempty"`
}

// клиент без cookie-хранилища и без следования редиректам — 302 отдаём как есть
var client = &http.Client{
	Timeout: 40 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func main() {
	wsURL := os.Getenv("AUTOMATION_AGENT_URL")
	if wsURL == "" {
		wsURL = defaultURL
	}
	token := os.Getenv("AUTOMATION_AGENT_TOKEN")
	if token == "" {
		log.Fatal("AUTOMATION_AGENT_TOKEN is required")
	}

	backoff := time.Second
	for {
		if err := connect(wsURL, token); err != nil {
			log.Printf("connection lost: %v; reconnecting in %s", err, backoff)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		} else {
			backoff = 30 * time.Second
		}
	}
}

type conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

func connect(wsURL, token string) error {
	header := map[string][]string{"Authorization": {"Bearer " + token}}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		return err
	}
	defer ws.Close()
	c := &conn{ws: ws}
	log.Printf("connected to %s", wsURL)

	ws.SetReadLimit(16 << 20)
	ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	ws.SetPingHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return ws.WriteMessage(websocket.PongMessage, nil)
	})

	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		if typ != websocket.TextMessage {
			continue
		}
		var req reqFrame
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		go c.handle(req)
	}
}

func (c *conn) handle(req reqFrame) {
	resp := c.perform(req)
	resp.ID = req.ID
	b, _ := json.Marshal(resp)
	c.writeMu.Lock()
	c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := c.ws.WriteMessage(websocket.TextMessage, b)
	c.writeMu.Unlock()
	if err != nil {
		log.Printf("write response: %v", err)
	}
}

func (c *conn) perform(req reqFrame) respFrame {
	u, err := url.Parse(req.URL)
	if err != nil {
		return respFrame{Error: "bad url"}
	}
	host := strings.ToLower(u.Hostname())
	if !allowedHosts[host] {
		return respFrame{Error: "host not allowed: " + host}
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	hr, err := http.NewRequest(req.Method, req.URL, body)
	if err != nil {
		return respFrame{Error: err.Error()}
	}
	for k, vs := range req.Headers {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	res, err := client.Do(hr)
	if err != nil {
		return respFrame{Error: err.Error()}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return respFrame{Error: err.Error()}
	}
	log.Printf("%s %s -> %d (%d b)", req.Method, u.Path, res.StatusCode, len(data))
	return respFrame{Status: res.StatusCode, Headers: res.Header, Body: data}
}
