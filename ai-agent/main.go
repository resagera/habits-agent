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
//   → {kind:"sessions", id, tool}          — список прошлых сессий CLI
//   → {kind:"session", id, tool, session_id} — содержимое одной сессии
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
//	AI_AGENT_CACHE     файл кэша индекса сессий (по умолчанию в ~/.cache)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const agentVersion = "1.5.0"

var httpClient = &http.Client{Timeout: 20 * time.Second}

func httpNewRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}
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
	Mode      string          `json:"mode,omitempty"` // '' | 'plan'
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

	// неподтверждённые результаты: run_id -> кадр (повтор после reconnect
	// и периодическим дослива-флашером)
	resultsMu sync.Mutex
	results   = map[int64]frame{}

	// текущее живое соединение: горутина прогона могла пережить reconnect,
	// поэтому шлёт через него, а не через соединение времени своего старта
	curMu sync.RWMutex
	cur   *conn

	// не больше двух прогонов одновременно
	runSem = make(chan struct{}, 2)

	// принятые/выполняющиеся прогоны: дедуп повторной доставки (сервер
	// дошлёт очередь на reconnect — процесс-то мог и не перезапускаться)
	// + отмена (canceled) и kill работающего процесса
	runsMu   sync.Mutex
	accepted = map[int64]bool{}
	canceled = map[int64]bool{}
	killers  = map[int64]func(){} // runID -> отмена контекста процесса
)

// sendCurrent шлёт кадр через актуальное соединение (если оно есть).
func sendCurrent(f frame) error {
	curMu.RLock()
	c := cur
	curMu.RUnlock()
	if c == nil {
		return errNoConn
	}
	return c.send(f)
}

var errNoConn = os.ErrClosed

// flusher периодически дошлёт неподтверждённые результаты: покрывает случай
// «результат готов, соединение уже новое» и «отправили, но сервер не сохранил
// (нет run_ack)». Сервер идемпотентен, повтор безопасен.
func flusher() {
	for range time.Tick(30 * time.Second) {
		resultsMu.Lock()
		pending := make([]frame, 0, len(results))
		for _, f := range results {
			pending = append(pending, f)
		}
		resultsMu.Unlock()
		for _, f := range pending {
			if err := sendCurrent(f); err == nil {
				log.Printf("run %d: result re-sent", f.RunID)
			}
		}
	}
}

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

	go flusher()
	go warmSessions() // индекс прошлых сессий: первый запрос из UI — уже по кэшу

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
	curMu.Lock()
	cur = c
	curMu.Unlock()
	log.Printf("connected to %s", wsURL)
	err = c.loop()
	curMu.Lock()
	if cur == c {
		cur = nil
	}
	curMu.Unlock()
	return err
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
		case "usage":
			go func(f frame) {
				res := toolUsage(f.Tool)
				_ = c.send(frame{Kind: "resp", ID: f.ID, OK: true, Result: res})
			}(f)
		case "sessions": // список прошлых сессий инструмента
			go func(f frame) {
				_ = c.send(frame{Kind: "resp", ID: f.ID, OK: true, Result: listSessions(f.Tool)})
			}(f)
		case "session": // содержимое одной сессии
			go func(f frame) {
				_ = c.send(frame{Kind: "resp", ID: f.ID, OK: true, Result: readSession(f.Tool, f.SessionID)})
			}(f)
		case "run":
			runsMu.Lock()
			dup := accepted[f.RunID]
			if !dup {
				accepted[f.RunID] = true
			}
			runsMu.Unlock()
			if dup { // повторная доставка (redelivery после reconnect)
				continue
			}
			go run(f)
		case "cancel":
			runsMu.Lock()
			canceled[f.RunID] = true
			kill := killers[f.RunID]
			runsMu.Unlock()
			if kill != nil {
				log.Printf("run %d: cancel — killing process", f.RunID)
				kill()
			} else {
				log.Printf("run %d: cancel noted", f.RunID)
			}
		case "run_ack":
			resultsMu.Lock()
			delete(results, f.RunID)
			resultsMu.Unlock()
			runsMu.Lock()
			delete(accepted, f.RunID)
			delete(canceled, f.RunID)
			runsMu.Unlock()
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
	// авторизация: реальный мини-запрос (как у claude) — единственный
	// надёжный способ; --ephemeral, чтобы не плодить сессии
	ctx2, cancel2 := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel2()
	cmd := exec.CommandContext(ctx2, bin, "exec", "--json", "--skip-git-repo-check", "--ephemeral", "-")
	cmd.Dir = dirs[0]
	cmd.Stdin = strings.NewReader("Reply with exactly: ok")
	out, err := cmd.Output()
	if err == nil && strings.Contains(string(out), `"agent_message"`) {
		st.Authorized = true
	} else {
		st.Error = "codex установлен, но не авторизован — выполните на машине: codex login"
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				st.Error += " · " + truncate(string(ee.Stderr), 300)
			}
		}
	}
	return st
}

// --- лимиты (аналог /usage в Claude Code и /status в Codex) ---

type usageWindow struct {
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at,omitempty"` // RFC3339
}

type usageInfo struct {
	Plan      string        `json:"plan,omitempty"`
	Windows   []usageWindow `json:"windows"`
	Note      string        `json:"note,omitempty"`
	Error     string        `json:"error,omitempty"`
	FetchedAt int64         `json:"fetched_at"`
}

func toolUsage(tool string) json.RawMessage {
	var u usageInfo
	switch tool {
	case "claude":
		u = usageClaude()
	case "codex":
		u = usageCodex()
	default:
		u.Error = "unknown tool"
	}
	u.FetchedAt = time.Now().Unix()
	if u.Windows == nil {
		u.Windows = []usageWindow{}
	}
	b, _ := json.Marshal(u)
	return b
}

// usageClaude — тот же источник, что у /usage: OAuth-эндпоинт Anthropic с
// токеном из ~/.claude/.credentials.json. Токен не покидает машину — наружу
// уходят только проценты и даты сброса.
func usageClaude() usageInfo {
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return usageInfo{Error: "нет ~/.claude/.credentials.json — войдите в Claude Code"}
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &creds) != nil || creds.ClaudeAiOauth.AccessToken == "" {
		return usageInfo{Error: "не удалось прочитать OAuth-токен Claude Code"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := httpNewRequest(ctx, "https://api.anthropic.com/api/oauth/usage")
	req.Header.Set("Authorization", "Bearer "+creds.ClaudeAiOauth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := httpClient.Do(req)
	if err != nil {
		return usageInfo{Error: "запрос usage не удался: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usageInfo{Error: "usage: HTTP " + resp.Status}
	}
	var out struct {
		Limits []struct {
			Kind     string  `json:"kind"`
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resets_at"`
			Scope    *struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return usageInfo{Error: "usage: некорректный ответ"}
	}
	u := usageInfo{}
	for _, l := range out.Limits {
		label := l.Kind
		switch l.Kind {
		case "session":
			label = "Сессия (5 ч)"
		case "weekly_all":
			label = "Неделя (все модели)"
		case "weekly_scoped":
			label = "Неделя"
			if l.Scope != nil && l.Scope.Model.DisplayName != "" {
				label = "Неделя (" + l.Scope.Model.DisplayName + ")"
			}
		}
		u.Windows = append(u.Windows, usageWindow{Label: label, Percent: l.Percent, ResetsAt: l.ResetsAt})
	}
	// план подписки — из claude auth status
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	if bin, err := exec.LookPath(claudeBin); err == nil {
		if out, err := exec.CommandContext(ctx2, bin, "auth", "status", "--json").Output(); err == nil {
			var st struct {
				SubscriptionType string `json:"subscriptionType"`
			}
			if json.Unmarshal(out, &st) == nil {
				u.Plan = st.SubscriptionType
			}
		}
	}
	return u
}

// usageCodex — свежего API нет; берём последний снапшот rate_limits из
// роллаутов сессий (~/.codex/sessions), который Codex пишет при каждом
// прогоне. В note — время снапшота.
func usageCodex() usageInfo {
	home, _ := os.UserHomeDir()
	files, err := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil || len(files) == 0 {
		return usageInfo{Error: "нет данных — выполните любую задачу Codex"}
	}
	// имена файлов датированы (rollout-YYYY-MM-DDTHH-MM-SS-...), сортировка
	// по пути = хронология; смотрим с конца, максимум 10 файлов
	sort.Strings(files)
	for i, checked := len(files)-1, 0; i >= 0 && checked < 10; i, checked = i-1, checked+1 {
		if u, ok := codexLimitsFromFile(files[i]); ok {
			return u
		}
	}
	return usageInfo{Error: "нет данных — выполните любую задачу Codex"}
}

func codexLimitsFromFile(path string) (usageInfo, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return usageInfo{}, false
	}
	type rlWindow struct {
		UsedPercent   float64 `json:"used_percent"`
		WindowMinutes int64   `json:"window_minutes"`
		ResetsAt      int64   `json:"resets_at"`
	}
	var last struct {
		ts string
		rl struct {
			Primary   *rlWindow `json:"primary"`
			Secondary *rlWindow `json:"secondary"`
			PlanType  string    `json:"plan_type"`
		}
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"rate_limits"`) {
			continue
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				RateLimits json.RawMessage `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || len(rec.Payload.RateLimits) == 0 {
			continue
		}
		if json.Unmarshal(rec.Payload.RateLimits, &last.rl) == nil {
			last.ts = rec.Timestamp
			found = true
		}
	}
	if !found {
		return usageInfo{}, false
	}
	u := usageInfo{Plan: last.rl.PlanType}
	add := func(w *rlWindow) {
		if w == nil {
			return
		}
		label := "Окно"
		switch {
		case w.WindowMinutes >= 10080:
			label = "Неделя"
		case w.WindowMinutes >= 240 && w.WindowMinutes <= 360:
			label = "5 часов"
		case w.WindowMinutes > 0:
			label = strconv.FormatInt(w.WindowMinutes/60, 10) + " ч"
		}
		var resets string
		if w.ResetsAt > 0 {
			resets = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		u.Windows = append(u.Windows, usageWindow{Label: label, Percent: w.UsedPercent, ResetsAt: resets})
	}
	add(last.rl.Primary)
	add(last.rl.Secondary)
	if last.ts != "" {
		u.Note = "по данным прогона " + last.ts
	}
	return u, true
}

// --- прогоны ---

func registerKiller(runID int64, cancel func()) {
	runsMu.Lock()
	killers[runID] = cancel
	runsMu.Unlock()
}

func unregisterKiller(runID int64) {
	runsMu.Lock()
	delete(killers, runID)
	runsMu.Unlock()
}

// runCanceled — прерван ли прогон пользователем (а не таймаутом).
func runCanceled(runID int64) bool {
	runsMu.Lock()
	defer runsMu.Unlock()
	return canceled[runID]
}

func dirAllowed(workdir string) bool {
	wd := filepath.Clean(workdir)
	for _, d := range dirs {
		if wd == d || strings.HasPrefix(wd, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// run выполняется в горутине и может пережить reconnect — все отправки идут
// через sendCurrent (актуальное соединение), а не через соединение старта.
func run(f frame) {
	runSem <- struct{}{}
	defer func() { <-runSem }()

	// отменили, пока ждали очередь семафора — не запускаем вовсе
	runsMu.Lock()
	wasCanceled := canceled[f.RunID]
	runsMu.Unlock()
	var result frame
	if wasCanceled {
		result = frame{OK: false, Error: "остановлено пользователем"}
	} else {
		result = execute(f)
	}
	result.Kind = "run_result"
	result.RunID = f.RunID
	resultsMu.Lock()
	results[f.RunID] = result
	resultsMu.Unlock()
	if err := sendCurrent(result); err != nil {
		log.Printf("run %d: send result failed (flusher re-sends): %v", f.RunID, err)
	}
}

func execute(f frame) frame {
	if !dirAllowed(f.Workdir) {
		return frame{OK: false, Error: "папка вне белого списка агента: " + f.Workdir}
	}
	if st, err := os.Stat(f.Workdir); err != nil || !st.IsDir() {
		return frame{OK: false, Error: "папка не существует: " + f.Workdir}
	}
	switch f.Tool {
	case "claude":
		return executeClaude(f)
	case "codex":
		return executeCodex(f)
	default:
		return frame{OK: false, Error: "инструмент не поддерживается: " + f.Tool}
	}
}

// sendLog — строка живого лога (best-effort: потеря не критична, финальный
// результат идёт надёжным путём run_result).
func sendLog(runID int64, line string) {
	_ = sendCurrent(frame{Kind: "run_log", RunID: runID, Output: line})
}

// toolSummary — короткая подпись входа инструмента для строки лога.
func toolSummary(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "cmd", "file_path", "path", "pattern", "url", "prompt", "description", "query"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncate(strings.Join(strings.Fields(v), " "), 140)
		}
	}
	return ""
}

func executeClaude(f frame) frame {
	bin, err := exec.LookPath(claudeBin)
	if err != nil {
		return frame{OK: false, Error: "claude не найден в PATH"}
	}

	// stream-json (+обязательный --verbose) — события хода выполнения на лету
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if f.Mode == "plan" {
		// режим «только план»: без правок файлов; bypass не добавляем
		args = append(args, "--permission-mode", "plan")
	} else if bypass {
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
	registerKiller(f.RunID, cancel)
	defer unregisterKiller(f.RunID)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = f.Workdir
	cmd.Stdin = strings.NewReader(f.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return frame{OK: false, Error: err.Error()}
	}

	log.Printf("run %d: claude in %s (model=%q resume=%v mode=%q)", f.RunID, f.Workdir, f.Model, f.SessionID != "", f.Mode)
	_ = sendCurrent(frame{Kind: "run_status", RunID: f.RunID, Status: "running"})
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return frame{OK: false, Error: err.Error()}
	}

	// потоковый разбор JSONL: события → живой лог, финальный result → итог
	var res struct {
		IsError      bool    `json:"is_error"`
		Result       string  `json:"result"`
		SessionID    string  `json:"session_id"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		NumTurns     int     `json:"num_turns"`
		Subtype      string  `json:"subtype"`
	}
	gotResult := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Name  string          `json:"name"`
					Text  string          `json:"text"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
			IsError      bool    `json:"is_error"`
			Result       string  `json:"result"`
			SessionID    string  `json:"session_id"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			NumTurns     int     `json:"num_turns"`
			Subtype      string  `json:"subtype"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "tool_use":
					s := toolSummary(b.Input)
					if s != "" {
						s = ": " + s
					}
					sendLog(f.RunID, "🔧 "+b.Name+s)
				case "text":
					if t := strings.TrimSpace(b.Text); t != "" {
						sendLog(f.RunID, "💬 "+truncate(strings.Join(strings.Fields(t), " "), 160))
					}
				}
			}
		case "result":
			res.IsError, res.Result, res.SessionID = ev.IsError, ev.Result, ev.SessionID
			res.TotalCostUSD, res.NumTurns, res.Subtype = ev.TotalCostUSD, ev.NumTurns, ev.Subtype
			gotResult = true
		}
	}
	runErr := cmd.Wait()
	elapsed := time.Since(start)

	meta := map[string]any{"elapsed_sec": int(elapsed.Seconds())}
	if branch, diffstat := gitInfo(f.Workdir); branch != "" {
		meta["branch"] = branch
		if diffstat != "" {
			meta["diffstat"] = diffstat
		}
	}

	if gotResult && res.SessionID != "" {
		meta["cost_usd"] = res.TotalCostUSD
		meta["num_turns"] = res.NumTurns
		mb, _ := json.Marshal(meta)
		if res.IsError || (runErr != nil && res.Result == "") {
			log.Printf("run %d: error after %s (%s)", f.RunID, elapsed, res.Subtype)
			return frame{OK: false, Error: truncate(firstNonEmpty(res.Result, stderr.String(), res.Subtype), 16_000),
				SessionID: res.SessionID, Meta: mb}
		}
		log.Printf("run %d: done in %s (turns=%d, cost=$%.4f)", f.RunID, elapsed, res.NumTurns, res.TotalCostUSD)
		return frame{OK: true, Output: truncate(res.Result, 512_000), SessionID: res.SessionID, Meta: mb}
	}

	// финального события нет (kill/таймаут/сбой) — ошибка из stderr
	mb, _ := json.Marshal(meta)
	msg := strings.TrimSpace(stderr.String())
	if runErr != nil {
		msg = firstNonEmpty(msg, runErr.Error())
		if ctx.Err() != nil {
			msg = "таймаут прогона (" + runTimeout.String() + ")"
			if runCanceled(f.RunID) {
				msg = "остановлено пользователем"
			}
		}
	}
	log.Printf("run %d: failed after %s: %s", f.RunID, elapsed, truncate(msg, 200))
	return frame{OK: false, Error: truncate(msg, 16_000), Meta: mb}
}

// executeCodex — прогон через Codex CLI (`codex exec`, JSONL-события).
// Контекст задачи: thread_id из события thread.started, продолжение —
// `codex exec resume <id>`. Модель по умолчанию — из config.toml машины.
func executeCodex(f frame) frame {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return frame{OK: false, Error: "codex не найден в PATH"}
	}

	args := []string{"exec"}
	if f.SessionID != "" {
		args = append(args, "resume", f.SessionID)
	}
	// --skip-git-repo-check: разрешённая папка не обязана быть git-репо
	args = append(args, "--json", "--skip-git-repo-check")
	if f.Mode == "plan" {
		// режим «только план»: чтение без правок; bypass не добавляем
		args = append(args, "--sandbox", "read-only")
	} else if bypass {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		args = append(args, "--sandbox", "workspace-write")
	}
	if f.Model != "" {
		args = append(args, "-m", f.Model)
	}
	args = append(args, filterParamsCodex(splitArgs(f.Params))...)
	args = append(args, "-") // промпт через stdin

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	registerKiller(f.RunID, cancel)
	defer unregisterKiller(f.RunID)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = f.Workdir
	cmd.Stdin = strings.NewReader(f.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return frame{OK: false, Error: err.Error()}
	}

	log.Printf("run %d: codex in %s (model=%q resume=%v mode=%q)", f.RunID, f.Workdir, f.Model, f.SessionID != "", f.Mode)
	_ = sendCurrent(frame{Kind: "run_status", RunID: f.RunID, Status: "running"})
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return frame{OK: false, Error: err.Error()}
	}

	// потоковый разбор JSONL: события → живой лог + финальные данные
	var threadID, lastMsg string
	var turns int
	var inTok, outTok int64
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Command string `json:"command"`
			} `json:"item"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			threadID = ev.ThreadID
		case "item.started":
			if ev.Item.Type == "command_execution" && ev.Item.Command != "" {
				sendLog(f.RunID, "🔧 "+truncate(strings.Join(strings.Fields(ev.Item.Command), " "), 140))
			}
		case "item.completed":
			if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
				lastMsg = ev.Item.Text
				sendLog(f.RunID, "💬 "+truncate(strings.Join(strings.Fields(ev.Item.Text), " "), 160))
			}
		case "turn.completed":
			turns++
			inTok += ev.Usage.InputTokens
			outTok += ev.Usage.OutputTokens
		}
	}
	runErr := cmd.Wait()
	elapsed := time.Since(start)

	meta := map[string]any{"elapsed_sec": int(elapsed.Seconds())}
	if branch, diffstat := gitInfo(f.Workdir); branch != "" {
		meta["branch"] = branch
		if diffstat != "" {
			meta["diffstat"] = diffstat
		}
	}
	if turns > 0 {
		meta["num_turns"] = turns
		meta["tokens_in"] = inTok
		meta["tokens_out"] = outTok
	}
	mb, _ := json.Marshal(meta)

	if runErr != nil || lastMsg == "" {
		msg := firstNonEmpty(strings.TrimSpace(stderr.String()), lastMsg)
		if runErr != nil {
			msg = firstNonEmpty(msg, runErr.Error())
			if ctx.Err() != nil {
				msg = "таймаут прогона (" + runTimeout.String() + ")"
				if runCanceled(f.RunID) {
					msg = "остановлено пользователем"
				}
			}
		}
		if msg == "" {
			msg = "codex не вернул ответ"
		}
		log.Printf("run %d: codex failed after %s: %s", f.RunID, elapsed, truncate(msg, 200))
		return frame{OK: false, Error: truncate(msg, 16_000), SessionID: threadID, Meta: mb}
	}
	log.Printf("run %d: codex done in %s (turns=%d)", f.RunID, elapsed, turns)
	return frame{OK: true, Output: truncate(lastMsg, 512_000), SessionID: threadID, Meta: mb}
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

// filterParams выбрасывает флаги claude, ломающие протокол (формат вывода,
// сессии). withValue — флаги, у которых значение идёт следующим аргументом.
func filterParams(args []string) []string {
	blocked := map[string]bool{
		"-p": true, "--print": true, "--output-format": true, "--input-format": true,
		"--resume": true, "-r": true, "--continue": true, "-c": true, "--session-id": true,
	}
	withValue := map[string]bool{
		"--output-format": true, "--input-format": true, "--resume": true,
		"-r": true, "--session-id": true,
	}
	return filterBlocked(args, blocked, withValue)
}

// filterParamsCodex — то же для codex exec (--json/-o/-C/-m задаёт агент).
func filterParamsCodex(args []string) []string {
	blocked := map[string]bool{
		"--json": true, "-o": true, "--output-last-message": true,
		"-C": true, "--cd": true, "-m": true, "--model": true, "-": true,
	}
	withValue := map[string]bool{
		"-o": true, "--output-last-message": true, "-C": true, "--cd": true,
		"-m": true, "--model": true,
	}
	return filterBlocked(args, blocked, withValue)
}

func filterBlocked(args []string, blocked, withValue map[string]bool) []string {
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
			// значение отдельным аргументом — только если флаг без '=значение'
			if name == a && withValue[name] {
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
