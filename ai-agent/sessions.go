// Прошлые сессии CLI-инструментов на машине: список (заголовок, папка, даты)
// и содержимое одной сессии по запросу.
//
//	Claude Code:  ~/.claude/projects/<slug>/<session-uuid>.jsonl
//	Codex CLI:    ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
//
// Файлы бывают по десятки мегабайт, поэтому список строится по кэшу
// (путь → mtime+size → метаданные): он прогревается при старте, сохраняется
// на диск и обновляется только для изменившихся файлов. Наружу отдаются
// только сессии, чья рабочая папка входит в белый список агента, — так же,
// как и для прогонов.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sessListLimit = 300        // сессий в списке (самые свежие)
	sessMaxLine   = 512 * 1024 // строки длиннее (большие tool_result) пропускаем
	sessTextCap   = 4000       // символов на одно сообщение
	sessBodyCap   = 300 * 1024 // символов на всю сессию (оставляем хвост)
	sessTitleCap  = 100        // символов в заголовке
)

// sessionMeta — карточка сессии для списка.
type sessionMeta struct {
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	Title     string `json:"title"`
	Workdir   string `json:"workdir"`
	Branch    string `json:"branch,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Msgs      int    `json:"msgs"`  // сообщений пользователя
	Bytes     int64  `json:"bytes"` // размер файла сессии
	Path      string `json:"-"`
}

// sessionMsg — одно сообщение сессии: реплика или строка вызова инструмента.
type sessionMsg struct {
	Role string `json:"role"` // user | assistant | tool
	Text string `json:"text"`
	TS   string `json:"ts,omitempty"`
}

type sessionList struct {
	Sessions []sessionMeta `json:"sessions"`
	Error    string        `json:"error,omitempty"`
}

type sessionDetail struct {
	Meta      sessionMeta  `json:"meta"`
	Messages  []sessionMsg `json:"messages"`
	Truncated bool         `json:"truncated"` // начало обрезано, показан хвост
	Error     string       `json:"error,omitempty"`
}

type cachedSession struct {
	MTime int64       `json:"mtime"`
	Size  int64       `json:"size"`
	Meta  sessionMeta `json:"meta"`
}

var (
	sessMu     sync.Mutex
	sessCache  = map[string]cachedSession{}
	sessLoaded bool
)

// --- индекс ---

func sessionCachePath() string {
	if v := os.Getenv("AI_AGENT_CACHE"); v != "" {
		return v
	}
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "habits-ai-agent", "sessions.json")
}

func loadSessionCache() {
	raw, err := os.ReadFile(sessionCachePath())
	if err != nil {
		return
	}
	var c map[string]cachedSession
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	for p, e := range c {
		e.Meta.Path = p
		sessCache[p] = e
	}
	log.Printf("sessions: cache loaded (%d files)", len(sessCache))
}

func saveSessionCache() {
	path := sessionCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(sessCache)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

func sessionFiles(tool string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var pattern string
	switch tool {
	case "claude":
		pattern = filepath.Join(home, ".claude", "projects", "*", "*.jsonl")
	case "codex":
		pattern = filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl")
	default:
		return nil
	}
	files, _ := filepath.Glob(pattern)
	return files
}

// indexSessions — список сессий инструмента: свежие сверху, только
// разрешённые папки. Изменившиеся файлы перечитываются, остальные — из кэша.
func indexSessions(tool string) []sessionMeta {
	sessMu.Lock()
	defer sessMu.Unlock()
	if !sessLoaded {
		loadSessionCache()
		sessLoaded = true
	}
	files := sessionFiles(tool)
	live := make(map[string]bool, len(files))
	dirty, scanned := false, 0
	out := make([]sessionMeta, 0, len(files))
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Size() == 0 {
			continue
		}
		live[p] = true
		c, ok := sessCache[p]
		if !ok || c.MTime != st.ModTime().Unix() || c.Size != st.Size() {
			c = cachedSession{MTime: st.ModTime().Unix(), Size: st.Size(), Meta: scanSession(tool, p, st)}
			sessCache[p] = c
			dirty, scanned = true, scanned+1
		}
		m := c.Meta
		m.Path, m.Tool = p, tool
		if m.ID == "" || !dirAllowed(m.Workdir) {
			continue
		}
		out = append(out, m)
	}
	for p, e := range sessCache { // файл удалён (или почищен) — забываем
		if !live[p] && e.Meta.Tool == tool {
			delete(sessCache, p)
			dirty = true
		}
	}
	if dirty {
		saveSessionCache()
	}
	if scanned > 0 {
		log.Printf("sessions: %s — %d files scanned, %d visible", tool, scanned, len(out))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	if len(out) > sessListLimit {
		out = out[:sessListLimit]
	}
	return out
}

// warmSessions — прогрев индекса при старте: первый запрос из приложения
// приходит уже на готовый кэш.
func warmSessions() {
	for _, tool := range []string{"claude", "codex"} {
		start := time.Now()
		n := len(indexSessions(tool))
		log.Printf("sessions: %s indexed — %d visible in %s", tool, n, time.Since(start).Round(time.Millisecond))
	}
}

func listSessions(tool string) json.RawMessage {
	res := sessionList{Sessions: indexSessions(tool)}
	if res.Sessions == nil {
		res.Sessions = []sessionMeta{}
	}
	if len(res.Sessions) == 0 {
		res.Error = "сессий не найдено (или все они вне разрешённых папок агента)"
	}
	b, _ := json.Marshal(res)
	return b
}

// readSession — содержимое одной сессии (только внутри разрешённых папок).
func readSession(tool, id string) json.RawMessage {
	var d sessionDetail
	var found bool
	for _, m := range indexSessions(tool) {
		if m.ID == id {
			d.Meta, found = m, true
			break
		}
	}
	if !found {
		d.Error = "сессия не найдена на машине"
	} else {
		switch tool {
		case "claude":
			d.Messages, d.Truncated = claudeMessages(d.Meta.Path)
		case "codex":
			d.Messages, d.Truncated = codexMessages(d.Meta.Path)
		}
		if len(d.Messages) == 0 {
			d.Error = "не удалось разобрать содержимое сессии"
		}
	}
	if d.Messages == nil {
		d.Messages = []sessionMsg{}
	}
	b, _ := json.Marshal(d)
	return b
}

// --- разбор метаданных ---

func scanSession(tool, path string, st os.FileInfo) sessionMeta {
	switch tool {
	case "claude":
		return scanClaudeSession(path, st)
	case "codex":
		return scanCodexSession(path, st)
	}
	return sessionMeta{}
}

func scanClaudeSession(path string, st os.FileInfo) sessionMeta {
	m := sessionMeta{
		ID:        strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Tool:      "claude",
		Bytes:     st.Size(),
		UpdatedAt: st.ModTime().UTC().Format(time.RFC3339),
	}
	var aiTitle, firstPrompt string
	eachLine(path, sessMaxLine, func(line []byte) {
		if bytes.Contains(line, []byte(`"isSidechain":true`)) {
			return // сессия сабагента внутри основной
		}
		switch {
		case bytes.Contains(line, []byte(`"type":"ai-title"`)):
			var ev struct {
				AiTitle string `json:"aiTitle"`
			}
			if json.Unmarshal(line, &ev) == nil && ev.AiTitle != "" {
				aiTitle = ev.AiTitle // последний в файле — самый свежий
			}
		case bytes.Contains(line, []byte(`"type":"user"`)):
			if bytes.Contains(line, []byte(`"tool_result"`)) {
				return // результат инструмента, а не реплика пользователя
			}
			m.Msgs++
			if m.Workdir != "" && firstPrompt != "" {
				return
			}
			var ev struct {
				Cwd       string `json:"cwd"`
				GitBranch string `json:"gitBranch"`
				Timestamp string `json:"timestamp"`
				Message   struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &ev) != nil {
				return
			}
			if m.Workdir == "" {
				m.Workdir, m.Branch, m.StartedAt = ev.Cwd, ev.GitBranch, ev.Timestamp
			}
			if firstPrompt == "" {
				firstPrompt = contentText(ev.Message.Content)
			}
		}
	})
	m.Title = sessionTitle(aiTitle, firstPrompt)
	return m
}

func scanCodexSession(path string, st os.FileInfo) sessionMeta {
	m := sessionMeta{
		Tool:      "codex",
		Bytes:     st.Size(),
		UpdatedAt: st.ModTime().UTC().Format(time.RFC3339),
	}
	var firstPrompt string
	eachLine(path, sessMaxLine, func(line []byte) {
		switch {
		case bytes.Contains(line, []byte(`"type":"session_meta"`)):
			var ev struct {
				Payload struct {
					SessionID string `json:"session_id"`
					Cwd       string `json:"cwd"`
					Timestamp string `json:"timestamp"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &ev) == nil {
				m.ID, m.Workdir, m.StartedAt = ev.Payload.SessionID, ev.Payload.Cwd, ev.Payload.Timestamp
			}
		case bytes.Contains(line, []byte(`"type":"user_message"`)):
			m.Msgs++
			if firstPrompt == "" {
				var ev struct {
					Payload struct {
						Message string `json:"message"`
					} `json:"payload"`
				}
				if json.Unmarshal(line, &ev) == nil {
					firstPrompt = ev.Payload.Message
				}
			}
		}
	})
	if m.ID == "" { // фолбэк: uuid в конце имени rollout-<ts>-<uuid>.jsonl
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if len(base) >= 36 {
			m.ID = base[len(base)-36:]
		}
	}
	m.Title = sessionTitle("", firstPrompt)
	return m
}

// --- разбор содержимого ---

func claudeMessages(path string) ([]sessionMsg, bool) {
	var b msgBuf
	eachLine(path, sessMaxLine, func(line []byte) {
		if bytes.Contains(line, []byte(`"isSidechain":true`)) {
			return
		}
		isUser := bytes.Contains(line, []byte(`"type":"user"`))
		if !isUser && !bytes.Contains(line, []byte(`"type":"assistant"`)) {
			return
		}
		if isUser && bytes.Contains(line, []byte(`"tool_result"`)) {
			return
		}
		var ev struct {
			Timestamp string `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if isUser {
			text := contentText(ev.Message.Content)
			if strings.HasPrefix(text, "<local-command") || strings.HasPrefix(text, "<command-") {
				return // служебный вывод локальной слэш-команды, не реплика
			}
			b.add("user", text, ev.Timestamp)
			return
		}
		var blocks []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return
		}
		for _, bl := range blocks {
			switch bl.Type {
			case "text":
				b.add("assistant", bl.Text, ev.Timestamp)
			case "tool_use":
				s := toolSummary(bl.Input)
				if s != "" {
					s = ": " + s
				}
				b.add("tool", bl.Name+s, ev.Timestamp)
			}
		}
	})
	return b.msgs, b.trunc
}

func codexMessages(path string) ([]sessionMsg, bool) {
	var b msgBuf
	eachLine(path, sessMaxLine, func(line []byte) {
		switch {
		case bytes.Contains(line, []byte(`"type":"user_message"`)),
			bytes.Contains(line, []byte(`"type":"agent_message"`)):
			var ev struct {
				Timestamp string `json:"timestamp"`
				Payload   struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &ev) != nil {
				return
			}
			role := "assistant"
			if ev.Payload.Type == "user_message" {
				role = "user"
			}
			b.add(role, ev.Payload.Message, ev.Timestamp)
		case bytes.Contains(line, []byte(`"type":"function_call"`)):
			var ev struct {
				Timestamp string `json:"timestamp"`
				Payload   struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &ev) != nil || ev.Payload.Name == "" {
				return
			}
			s := toolSummary(json.RawMessage(ev.Payload.Arguments))
			if s == "" {
				s = strings.Join(strings.Fields(ev.Payload.Arguments), " ")
			}
			if s != "" {
				s = ": " + truncate(s, 140)
			}
			b.add("tool", ev.Payload.Name+s, ev.Timestamp)
		}
	})
	return b.msgs, b.trunc
}

// msgBuf копит сообщения в пределах лимита, выкидывая самые старые: у длинной
// сессии полезнее хвост — именно его продолжает пользователь.
type msgBuf struct {
	msgs  []sessionMsg
	total int
	trunc bool
}

func (b *msgBuf) add(role, text, ts string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	text = truncate(text, sessTextCap)
	b.msgs = append(b.msgs, sessionMsg{Role: role, Text: text, TS: ts})
	b.total += len(text)
	for b.total > sessBodyCap && len(b.msgs) > 1 {
		b.total -= len(b.msgs[0].Text)
		b.msgs = b.msgs[1:]
		b.trunc = true
	}
}

// contentText — текст сообщения: строка или блоки {type:text}.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

var sessTagRe = regexp.MustCompile(`<[^>\n]{1,60}>`)

// sessionTitle — заголовок сессии: авто-заголовок Claude Code либо первая
// реплика пользователя без служебных тегов.
func sessionTitle(aiTitle, firstPrompt string) string {
	t := strings.TrimSpace(aiTitle)
	if t == "" {
		t = strings.TrimSpace(sessTagRe.ReplaceAllString(firstPrompt, " "))
	}
	t = strings.Join(strings.Fields(t), " ")
	if t == "" {
		return "(без заголовка)"
	}
	if r := []rune(t); len(r) > sessTitleCap {
		return string(r[:sessTitleCap]) + "…"
	}
	return t
}

// eachLine читает файл построчно без ограничения буфера сканера: строки
// длиннее maxLine (огромные tool_result) отдаются обрезанными и обычно не
// разбираются. Срез валиден только внутри вызова fn.
func eachLine(path string, maxLine int, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(buf) < maxLine {
				buf = append(buf, chunk...)
			}
			continue
		}
		line := chunk
		if len(buf) > 0 {
			if len(buf) < maxLine {
				buf = append(buf, chunk...)
			}
			line = buf
		}
		if len(bytes.TrimSpace(line)) > 0 {
			fn(line)
		}
		buf = buf[:0]
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("sessions: read %s: %v", path, err)
			}
			return
		}
	}
}
