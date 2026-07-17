// habits-files-agent — файловый агент для страницы «My Files».
// Держит исходящий WebSocket к бэкенду (у домашних машин нет внешнего IP)
// и выполняет операции над файлами только в разрешённых папках.
//
// Каждая папка открывается в режиме ro (только чтение) или rw (чтение и
// запись). Любой путь запроса проверяется на принадлежность одной из папок —
// выход за их пределы (в т.ч. через .. и симлинки) запрещён.
//
// Конфиг через переменные окружения:
//
//	FILES_AGENT_URL    wss://host/app/habits/api/v1/files/agent
//	FILES_AGENT_TOKEN  токен машины (выдаётся в UI при добавлении)
//	FILES_AGENT_ROOTS  список папок «путь:режим», разделённых ';'
//	                   например: /home/user/media:ro;/data/box:rw
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type root struct {
	Path string `json:"path"`
	Mode string `json:"mode"` // ro | rw
}

var roots []root

type request struct {
	ID     uint64 `json:"id"`
	Op     string `json:"op"`
	Path   string `json:"path"`
	To     string `json:"to"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Data   []byte `json:"data"`
	Trunc  bool   `json:"trunc"`
	IsDir  bool   `json:"is_dir"`
}

type response struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	EOF    bool            `json:"eof,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

func main() {
	url := os.Getenv("FILES_AGENT_URL")
	token := os.Getenv("FILES_AGENT_TOKEN")
	if url == "" || token == "" {
		log.Fatal("FILES_AGENT_URL and FILES_AGENT_TOKEN are required")
	}
	roots = parseRoots(os.Getenv("FILES_AGENT_ROOTS"))
	if len(roots) == 0 {
		log.Fatal("FILES_AGENT_ROOTS is empty — nothing to serve")
	}
	for _, r := range roots {
		log.Printf("root %s (%s)", r.Path, r.Mode)
	}

	backoff := time.Second
	for {
		if err := connect(url, token); err != nil {
			log.Printf("connection lost: %v; reconnecting in %s", err, backoff)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// parseRoots разбирает «путь:режим;путь:режим». Режим по умолчанию — ro.
func parseRoots(spec string) []root {
	var out []root
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		path, mode := part, "ro"
		if i := strings.LastIndexByte(part, ':'); i > 0 {
			if m := part[i+1:]; m == "ro" || m == "rw" {
				path, mode = part[:i], m
			}
		}
		abs, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			log.Printf("skip bad root %q: %v", path, err)
			continue
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			log.Printf("skip root %q: not a directory", abs)
			continue
		}
		out = append(out, root{Path: abs, Mode: mode})
	}
	return out
}

type conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

func connect(url, token string) error {
	header := map[string][]string{"Authorization": {"Bearer " + token}}
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return err
	}
	defer ws.Close()
	c := &conn{ws: ws}

	hello, _ := json.Marshal(map[string]any{"roots": roots})
	if err := c.writeText(hello); err != nil {
		return err
	}
	log.Printf("connected to %s", url)

	ws.SetReadLimit(4 << 20)
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
		var req request
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		go c.handle(req) // параллельно: стрим не блокирует навигацию
	}
}

func (c *conn) writeText(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

// writeReadResult шлёт бинарный кадр (id + данные) и затем JSON-подтверждение.
func (c *conn) writeReadResult(id uint64, data []byte, eof bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	frame := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(frame[:8], id)
	copy(frame[8:], data)
	c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return err
	}
	ack, _ := json.Marshal(response{ID: id, OK: true, EOF: eof})
	c.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, ack)
}

func (c *conn) reply(id uint64, result any) {
	raw, _ := json.Marshal(result)
	b, _ := json.Marshal(response{ID: id, OK: true, Result: raw})
	_ = c.writeText(b)
}

func (c *conn) fail(id uint64, err error) {
	b, _ := json.Marshal(response{ID: id, OK: false, Error: err.Error()})
	_ = c.writeText(b)
}

func (c *conn) handle(req request) {
	switch req.Op {
	case "list":
		c.handleList(req)
	case "stat":
		c.handleStat(req)
	case "read":
		c.handleRead(req)
	case "write":
		c.handleWrite(req)
	case "mkdir":
		c.handleMkdir(req)
	case "rename":
		c.handleRename(req)
	case "remove":
		c.handleRemove(req)
	default:
		c.fail(req.ID, errors.New("unknown op"))
	}
}

var errDenied = errors.New("access denied")

// resolve проверяет, что путь лежит внутри разрешённой папки, и возвращает
// её режим. Симлинки разворачиваются и проверяются повторно.
func resolve(p string) (abs, mode string, err error) {
	if p == "" {
		return "", "", errDenied
	}
	abs = filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		return "", "", errDenied
	}
	within := func(target string) (string, bool) {
		for _, r := range roots {
			if target == r.Path || strings.HasPrefix(target, r.Path+string(filepath.Separator)) {
				return r.Mode, true
			}
		}
		return "", false
	}
	m, ok := within(abs)
	if !ok {
		return "", "", errDenied
	}
	// защита от симлинк-побега: если реальный путь существует, он тоже
	// обязан оставаться внутри разрешённой папки
	if real, e := filepath.EvalSymlinks(abs); e == nil {
		if _, ok := within(real); !ok {
			return "", "", errDenied
		}
	}
	return abs, m, nil
}

func (c *conn) handleList(req request) {
	// пустой путь — верхний уровень: перечисляем сами разрешённые папки
	if req.Path == "" || req.Path == "/" {
		entries := make([]entry, 0, len(roots))
		for _, r := range roots {
			st, err := os.Stat(r.Path)
			if err != nil {
				continue
			}
			entries = append(entries, entry{
				Name: r.Path, Path: r.Path, IsDir: true, ModTime: st.ModTime().Unix(),
			})
		}
		c.reply(req.ID, map[string]any{"entries": entries, "top": true})
		return
	}
	abs, _, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	dir, err := os.ReadDir(abs)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	entries := make([]entry, 0, len(dir))
	for _, d := range dir {
		info, err := d.Info()
		if err != nil {
			continue
		}
		entries = append(entries, entry{
			Name:    d.Name(),
			Path:    filepath.Join(abs, d.Name()),
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // папки выше файлов
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	c.reply(req.ID, map[string]any{"entries": entries})
}

func (c *conn) handleStat(req request) {
	abs, _, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	c.reply(req.ID, entry{
		Name: st.Name(), Path: abs, IsDir: st.IsDir(),
		Size: st.Size(), ModTime: st.ModTime().Unix(),
	})
}

func (c *conn) handleRead(req request) {
	abs, _, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	defer f.Close()
	length := req.Length
	if length <= 0 || length > 1<<20 {
		length = 512 * 1024
	}
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, req.Offset)
	eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if err != nil && !eof {
		c.fail(req.ID, err)
		return
	}
	if e := c.writeReadResult(req.ID, buf[:n], eof); e != nil {
		log.Printf("read reply: %v", e)
	}
}

func (c *conn) handleWrite(req request) {
	abs, mode, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	if mode != "rw" {
		c.fail(req.ID, errors.New("folder is read-only"))
		return
	}
	flag := os.O_WRONLY | os.O_CREATE
	if req.Trunc {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	defer f.Close()
	if len(req.Data) > 0 {
		if _, err := f.WriteAt(req.Data, req.Offset); err != nil {
			c.fail(req.ID, err)
			return
		}
	}
	c.reply(req.ID, map[string]any{"written": len(req.Data)})
}

func (c *conn) handleMkdir(req request) {
	abs, mode, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	if mode != "rw" {
		c.fail(req.ID, errors.New("folder is read-only"))
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		c.fail(req.ID, err)
		return
	}
	c.reply(req.ID, map[string]any{"ok": true})
}

func (c *conn) handleRename(req request) {
	src, mode, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	dst, mode2, err := resolve(req.To)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	if mode != "rw" || mode2 != "rw" {
		c.fail(req.ID, errors.New("folder is read-only"))
		return
	}
	if err := os.Rename(src, dst); err != nil {
		c.fail(req.ID, err)
		return
	}
	c.reply(req.ID, map[string]any{"ok": true})
}

func (c *conn) handleRemove(req request) {
	abs, mode, err := resolve(req.Path)
	if err != nil {
		c.fail(req.ID, err)
		return
	}
	if mode != "rw" {
		c.fail(req.ID, errors.New("folder is read-only"))
		return
	}
	// сам корень удалять нельзя
	for _, r := range roots {
		if abs == r.Path {
			c.fail(req.ID, errDenied)
			return
		}
	}
	remove := os.Remove
	if req.IsDir {
		remove = os.RemoveAll
	}
	if err := remove(abs); err != nil {
		c.fail(req.ID, err)
		return
	}
	c.reply(req.ID, map[string]any{"ok": true})
}
