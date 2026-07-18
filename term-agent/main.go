// habits-term-agent — shell-агент для страницы Terminal. Держит исходящий
// WebSocket к бэкенду (у домашних машин нет внешнего IP) и по запросу открывает
// PTY-сессии: веб-консоль к своей машине из любой точки.
//
// ВНИМАНИЕ: агент даёт полный доступ к shell на этой машине под пользователем,
// от которого запущен. Ставьте только на свои машины, храните токен в секрете.
//
// Конфиг через переменные окружения:
//
//	TERM_AGENT_URL    wss://host/app/habits/api/v1/terminal/agent
//	TERM_AGENT_TOKEN  токен машины (выдаётся в UI при добавлении)
//	TERM_AGENT_SHELL  shell (по умолчанию $SHELL или /bin/bash)
//	TERM_AGENT_DIR    стартовый каталог (по умолчанию $HOME)
package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type control struct {
	T    string `json:"t"`
	SID  uint64 `json:"sid"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

var (
	shell   string
	workDir string
)

func main() {
	url := os.Getenv("TERM_AGENT_URL")
	token := os.Getenv("TERM_AGENT_TOKEN")
	if url == "" || token == "" {
		log.Fatal("TERM_AGENT_URL and TERM_AGENT_TOKEN are required")
	}
	shell = os.Getenv("TERM_AGENT_SHELL")
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		shell = "/bin/bash"
	}
	workDir = os.Getenv("TERM_AGENT_DIR")
	if workDir == "" {
		workDir = os.Getenv("HOME")
	}
	log.Printf("shell=%s dir=%s", shell, workDir)

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

type conn struct {
	ws       *websocket.Conn
	writeMu  sync.Mutex
	mu       sync.Mutex
	sessions map[uint64]*session
}

type session struct {
	sid  uint64
	ptmx *os.File
	cmd  *exec.Cmd
}

func connect(url, token string) error {
	header := map[string][]string{"Authorization": {"Bearer " + token}}
	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return err
	}
	defer ws.Close()
	c := &conn{ws: ws, sessions: map[uint64]*session{}}
	defer c.closeAll()
	log.Printf("connected to %s", url)

	ws.SetReadLimit(1 << 20)
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
		switch typ {
		case websocket.BinaryMessage:
			if len(data) < 8 {
				continue
			}
			sid := binary.BigEndian.Uint64(data[:8])
			c.mu.Lock()
			s := c.sessions[sid]
			c.mu.Unlock()
			if s != nil {
				s.ptmx.Write(data[8:]) // stdin в PTY
			}
		case websocket.TextMessage:
			var msg control
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			c.handle(msg)
		}
	}
}

func (c *conn) handle(msg control) {
	switch msg.T {
	case "open":
		c.open(msg)
	case "resize":
		c.mu.Lock()
		s := c.sessions[msg.SID]
		c.mu.Unlock()
		if s != nil {
			pty.Setsize(s.ptmx, &pty.Winsize{Rows: msg.Rows, Cols: msg.Cols})
		}
	case "close":
		c.mu.Lock()
		s := c.sessions[msg.SID]
		delete(c.sessions, msg.SID)
		c.mu.Unlock()
		if s != nil {
			s.kill()
		}
	}
}

func (c *conn) open(msg control) {
	cmd := exec.Command(shell, "-l")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("pty start: %v", err)
		c.writeControl(control{T: "exit", SID: msg.SID})
		return
	}
	if msg.Cols > 0 && msg.Rows > 0 {
		pty.Setsize(ptmx, &pty.Winsize{Rows: msg.Rows, Cols: msg.Cols})
	}
	s := &session{sid: msg.SID, ptmx: ptmx, cmd: cmd}
	c.mu.Lock()
	c.sessions[msg.SID] = s
	c.mu.Unlock()

	// PTY stdout → бэкенд (бинарные кадры [sid][data])
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				frame := make([]byte, 8+n)
				binary.BigEndian.PutUint64(frame[:8], msg.SID)
				copy(frame[8:], buf[:n])
				if e := c.writeBinary(frame); e != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		// shell завершился или PTY закрыт
		c.mu.Lock()
		delete(c.sessions, msg.SID)
		c.mu.Unlock()
		s.kill()
		c.writeControl(control{T: "exit", SID: msg.SID})
	}()
}

func (c *conn) writeBinary(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (c *conn) writeControl(v control) {
	data, _ := json.Marshal(v)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
	c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *conn) closeAll() {
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = map[uint64]*session{}
	c.mu.Unlock()
	for _, s := range sessions {
		s.kill()
	}
}

func (s *session) kill() {
	s.ptmx.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	go s.cmd.Wait() // пожинаем зомби
}
