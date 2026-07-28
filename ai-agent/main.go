// habits-ai-agent — coding-агент для страницы «AI»: запускает Claude Code
// (print mode) на домашней машине по задачам из мини-приложения. Держит
// исходящий WebSocket к бэкенду (у домашних машин нет внешнего IP).
//
// Безопасность: задачи выполняются ТОЛЬКО в папках из белого списка
// AI_AGENT_DIRS (выход за пределы, в т.ч. через .., запрещён). Режим прав
// Claude Code по умолчанию — полный bypass (--dangerously-skip-permissions),
// отключается AI_AGENT_BYPASS=0.
//
// Протокол (JSON-кадры):
//   ← hello {dirs, version}                — первый кадр после подключения
//   → {kind:"check", id, tool}             — проверить инструмент
//   ← {kind:"resp", id, ok, result}        — {installed, authorized, version, error}
//   → {kind:"run", run_id, tool, workdir, model, params, prompt, session_id}
//   ← {kind:"run_status", run_id, status:"running"}
//   ← {kind:"run_result", run_id, ok, output, error, session_id, meta}
//   → {kind:"run_ack", run_id}             — подтверждение: результат сохранён
// Неподтверждённые результаты хранятся в памяти и повторяются после
// переподключения (сервер идемпотентен).
//
// Конфиг через переменные окружения:
//
//	AI_AGENT_URL       wss://host/app/habits/api/v1/ai/agent
//	AI_AGENT_TOKEN     токен машины (выдаётся в UI при добавлении)
//	AI_AGENT_DIRS      разрешённые папки через ';'
//	AI_AGENT_BYPASS    1 (по умолчанию) — --dangerously-skip-permissions
//	AI_AGENT_CLAUDE    путь к бинарнику claude (по умолчанию из PATH)
//	AI_AGENT_TIMEOUT   таймаут одного прогона в минутах (по умолчанию 60)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const agentVersion = "1.0.0"
const defaultURL = "wss://telegram.resager.ru/app/habits/api/v1/ai/agent"

type frame struct {
	Kind      string          `json:"kind"`
	ID        uint64          `json:"id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	OK        bool            `json:"ok,omitempty"`
	Error     string          `json:"error,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	RunID     int64           `json:"run_id,omitempty"`
	Workdir   string          `json:"workdir,omitempty"`
	Model     string          `json:"model,omitempty"`
	Params    string          `json:"params,omitempty"`
	Prompt    string          `json:"prompt,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Status    string          `json:"status,omitempty"`
	Output    string          `json:"output,omitempty"`
	Meta      json.RawMessage `json:"meta,omitempty"`
}

type hello struct {
	Dirs    []string `json:"dirs"`
	Version string   `json:"version"`
}

var (
	dirs       []string
	bypass     = true
	claudeBin  = "claude"
	runTimeout = 60 * time.Minute

	// неподтверждённые результаты: run_id -> кадр (повтор после reconnect)
	resultsMu sync.Mutex
	results   = map[int64]frame{}

	// не больше двух прогонов одновременно
	runSem = make(chan struct{}, 2)
)

func main() {
	wsURL := os.Getenv("AI_AGENT_URL")
	if wsURL == "" {
		wsURL = defaultURL
	}
	token := os.Getenv("AI_AGENT_TOKEN")
	if token == "" {
		log.Fatal("AI_AGENT_TOKEN is required")
	}
	for _, d := range strings.Split(os.Getenv("AI_AGENT_DIRS"), ";") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			log.Fatalf("bad dir %q: %v", d, err)
		}
		dirs = append(dirs, filepath.Clean(abs))
	}
	if len(dirs) == 0 {
		log.Fatal("AI_AGENT_DIRS is empty — nothing to serve")
	}
	if os.Getenv("AI_AGENT_BYPASS") == "0" {
		bypass = false
	}
	if v := os.Getenv("AI_AGENT_CLAUDE"); v != "" {
		claudeBin = v
	}
	if v, err := strconv.Atoi(os.Getenv("AI_AGENT_TIMEOUT")); err == nil && v > 0 {
		runTimeout = time.Duration(v) * time.Minute
	}
	for _, d := range dirs {
		log.Printf("dir %s", d)
	}
	log.Printf("bypass=%v timeout=%s", bypass, runTimeout)

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

func (c *conn) send(f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, data)
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
	return c.loop()
}

func (c *conn) loop() error {
	// hello: разрешённые папки + версия агента
	hb, _ := json.Marshal(hello{Dirs: dirs, Version: agentVersion})
	c.writeMu.Lock()
	c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := c.ws.WriteMessage(websocket.TextMessage, hb)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}
	// повтор неподтверждённых результатов после переподключения
	resultsMu.Lock()
	for _, f := range results {
		_ = c.send(f)
	}
	resultsMu.Unlock()

	c.ws.SetReadLimit(1 << 20)
	c.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	c.ws.SetPingHandler(func(string) error {
		c.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return c.ws.WriteMessage(websocket.PongMessage, nil)
	})

	for {
		typ, data, err := c.ws.ReadMessage()
		if err != nil {
			return err
		}
		c.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		if typ != websocket.TextMessage {
			continue
		}
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.Kind {
		case "check":
			go func(f frame) {
				res := checkTool(f.Tool)
				_ = c.send(frame{Kind: "resp", ID: f.ID, OK: true, Result: res})
			}(f)
		case "run":
			go c.run(f)
		case "run_ack":
			resultsMu.Lock()
			delete(results, f.RunID)
			resultsMu.Unlock()
		}
	}
}

// --- проверка инструментов ---

type toolStatus struct {
	Installed  bool   `json:"installed"`
	Authorized bool   `json:"authorized"`
	Version    string `json:"version"`
	Error      string `json:"error,omitempty"`
	CheckedAt  int64  `json:"checked_at"`
}

func checkTool(tool string) json.RawMessage {
	st := toolStatus{CheckedAt: time.Now().Unix()}
	switch tool {
	case "claude":
		st = checkClaude()
	case "codex":
		st = checkCodex()
	default:
		st.Error = "unknown tool"
	}
	st.CheckedAt = time.Now().Unix()
	b, _ := json.Marshal(st)
	return b
}

func checkClaude() toolStatus {
	st := toolStatus{}
	bin, err := exec.LookPath(claudeBin)
	if err != nil {
		st.Error = "claude не найден в PATH — установите Claude Code"
		return st
	}
	st.Installed = true
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "--version").Output(); err == nil {
		st.Version = strings.TrimSpace(string(out))
	}
	// авторизация: реальный мини-запрос (haiku) — единственный надёжный способ
	ctx2, cancel2 := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel2()
	cmd := exec.CommandContext(ctx2, bin, "-p", "--model", "haiku", "--output-format", "json")
	cmd.Dir = dirs[0]
	cmd.Stdin = strings.NewReader("Reply with exactly: ok")
	out, err := cmd.Output()
	var res struct {
		IsError bool `json:"is_error"`
	}
	if err == nil && json.Unmarshal(out, &res) == nil && !res.IsError {
		st.Authorized = true
	} else {
		st.Error = "claude установлен, но не авторизован — выполните на машине: claude (и войдите)"
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				st.Error += " · " + truncate(string(ee.Stderr), 300)
			}
		}
	}
	return st
}

func checkCodex() toolStatus {
	st := toolStatus{}
	bin, err := exec.LookPath("codex")
	if err != nil {
		st.Error = "codex не найден в PATH — установите Codex CLI"
		return st
	}
	st.Installed = true
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "--version").Output(); err == nil {
		st.Version = strings.TrimSpace(string(out))
	}
	// эвристика: наличие файла авторизации (запуск задач для codex пока не реализован)
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); err == nil {
		st.Authorized = true
	} else {
		st.Error = "codex установлен, но не авторизован — выполните на машине: codex login"
	}
	return st
}

// --- прогоны ---

func dirAllowed(workdir string) bool {
	wd := filepath.Clean(workdir)
	for _, d := range dirs {
		if wd == d || strings.HasPrefix(wd, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (c *conn) run(f frame) {
	runSem <- struct{}{}
	defer func() { <-runSem }()

	result := c.execute(f)
	result.Kind = "run_result"
	result.RunID = f.RunID
	resultsMu.Lock()
	results[f.RunID] = result
	resultsMu.Unlock()
	if err := c.send(result); err != nil {
		log.Printf("run %d: send result failed (queued for resend): %v", f.RunID, err)
	}
}

func (c *conn) execute(f frame) frame {
	if f.Tool != "claude" {
		return frame{OK: false, Error: "инструмент не поддерживается: " + f.Tool}
	}
	if !dirAllowed(f.Workdir) {
		return frame{OK: false, Error: "папка вне белого списка агента: " + f.Workdir}
	}
	if st, err := os.Stat(f.Workdir); err != nil || !st.IsDir() {
		return frame{OK: false, Error: "папка не существует: " + f.Workdir}
	}
	bin, err := exec.LookPath(claudeBin)
	if err != nil {
		return frame{OK: false, Error: "claude не найден в PATH"}
	}

	args := []string{"-p", "--output-format", "json"}
	if bypass {
		args = append(args, "--dangerously-skip-permissions")
	}
	if f.Model != "" {
		args = append(args, "--model", f.Model)
	}
	if f.SessionID != "" {
		args = append(args, "--resume", f.SessionID)
	}
	args = append(args, filterParams(splitArgs(f.Params))...)

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = f.Workdir
	cmd.Stdin = strings.NewReader(f.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Printf("run %d: claude in %s (model=%q resume=%v)", f.RunID, f.Workdir, f.Model, f.SessionID != "")
	_ = c.send(frame{Kind: "run_status", RunID: f.RunID, Status: "running"})
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	meta := map[string]any{"elapsed_sec": int(elapsed.Seconds())}
	if branch, diffstat := gitInfo(f.Workdir); branch != "" {
		meta["branch"] = branch
		if diffstat != "" {
			meta["diffstat"] = diffstat
		}
	}

	// разбор JSON print-режима: result, session_id, стоимость и т.п.
	var out struct {
		IsError      bool    `json:"is_error"`
		Result       string  `json:"result"`
		SessionID    string  `json:"session_id"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		NumTurns     int     `json:"num_turns"`
		Subtype      string  `json:"subtype"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err == nil && out.SessionID != "" {
		meta["cost_usd"] = out.TotalCostUSD
		meta["num_turns"] = out.NumTurns
		mb, _ := json.Marshal(meta)
		if out.IsError || (runErr != nil && out.Result == "") {
			log.Printf("run %d: error after %s (%s)", f.RunID, elapsed, out.Subtype)
			return frame{OK: false, Error: truncate(firstNonEmpty(out.Result, stderr.String(), out.Subtype), 16_000),
				SessionID: out.SessionID, Meta: mb}
		}
		log.Printf("run %d: done in %s (turns=%d, cost=$%.4f)", f.RunID, elapsed, out.NumTurns, out.TotalCostUSD)
		return frame{OK: true, Output: truncate(out.Result, 512_000), SessionID: out.SessionID, Meta: mb}
	}

	// JSON не распарсился — вернуть сырой вывод как ошибку
	mb, _ := json.Marshal(meta)
	msg := firstNonEmpty(strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()))
	if runErr != nil {
		msg = firstNonEmpty(msg, runErr.Error())
		if ctx.Err() != nil {
			msg = "таймаут прогона (" + runTimeout.String() + ")"
		}
	}
	log.Printf("run %d: failed after %s: %s", f.RunID, elapsed, truncate(msg, 200))
	return frame{OK: false, Error: truncate(msg, 16_000), Meta: mb}
}

// gitInfo — ветка и краткий diffstat рабочей папки (best-effort).
func gitInfo(dir string) (branch, diffstat string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--stat").Output(); err == nil {
		diffstat = truncate(strings.TrimSpace(string(out)), 4000)
	}
	return
}

// splitArgs — разбор строки доп. параметров с учётом одинарных и двойных кавычек.
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

// filterParams выбрасывает флаги, ломающие протокол (формат вывода, сессии).
func filterParams(args []string) []string {
	blocked := map[string]bool{
		"-p": true, "--print": true, "--output-format": true, "--input-format": true,
		"--resume": true, "-r": true, "--continue": true, "-c": true, "--session-id": true,
	}
	var out []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		name := a
		if i := strings.IndexByte(a, '='); i > 0 {
			name = a[:i]
		}
		if blocked[name] {
			// у заблокированного флага без '=' значение идёт следующим аргументом
			if name == a && (name == "--output-format" || name == "--input-format" ||
				name == "--resume" || name == "-r" || name == "--session-id" || name == "--model") {
				skipNext = true
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(обрезано)"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
