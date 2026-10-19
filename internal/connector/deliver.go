package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Printer writes each block to w, for testing and for plain terminals.
type Printer struct {
	mu sync.Mutex
	W  io.Writer
}

func (p *Printer) Name() string { return "stdout" }

func (p *Printer) Deliver(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.W, text+"\n")
}

func (p *Printer) Connected(string) {}

// JSONLines hands blocks to a pi or omp extension as JSON lines on w:
// {"type":"connected","member":...} and {"type":"deliver","text":...}.
// The extension decides when the agent can take them.
type JSONLines struct {
	mu sync.Mutex
	W  io.Writer
}

func (j *JSONLines) Name() string { return "extension" }

func (j *JSONLines) write(v any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, _ := json.Marshal(v)
	j.W.Write(append(b, '\n'))
}

func (j *JSONLines) Deliver(text string) {
	j.write(map[string]string{"type": "deliver", "text": text})
}

func (j *JSONLines) Connected(member string) {
	j.write(map[string]string{"type": "connected", "member": member})
}

// ClaudeSocket writes blocks into a Claude Code session through the
// session's messaging socket, as a user message, while the session is
// idle. Blocks that arrive mid-turn wait and go together at the next idle
// moment.
type ClaudeSocket struct {
	PID      int
	Socket   string
	Token    string
	Registry string
	Log      *log.Logger

	mu      sync.Mutex
	pending []string
	kick    chan struct{}
}

// claudeSession is the part of ~/.claude/sessions/PID.json read here.
type claudeSession struct {
	PID                 int    `json:"pid"`
	Status              string `json:"status"`
	MessagingSocketPath string `json:"messagingSocketPath"`
}

// NewClaudeSocket finds the messaging socket of the Claude Code session
// pid, which must be the session this process runs under.
func NewClaudeSocket(pid int, lg *log.Logger) (*ClaudeSocket, error) {
	sock := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	if sock == "" {
		return nil, errors.New("CLAUDE_CODE_MESSAGING_SOCKET is not set; this Claude Code is too old to take messages from the mesh")
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude")
	}
	c := &ClaudeSocket{PID: pid, Socket: sock, Token: os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"),
		Registry: filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", pid)), Log: lg, kick: make(chan struct{}, 1)}
	s, err := c.session()
	if err != nil {
		return nil, err
	}
	if s.PID != pid || s.MessagingSocketPath != sock {
		return nil, fmt.Errorf("%s does not describe this session's socket", c.Registry)
	}
	return c, nil
}

func (c *ClaudeSocket) Name() string { return "claude-socket" }

func (c *ClaudeSocket) Connected(string) {}

func (c *ClaudeSocket) session() (*claudeSession, error) {
	b, err := os.ReadFile(c.Registry)
	if err != nil {
		return nil, err
	}
	var s claudeSession
	return &s, json.Unmarshal(b, &s)
}

func (c *ClaudeSocket) Deliver(text string) {
	c.mu.Lock()
	c.pending = append(c.pending, text)
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run pushes pending blocks whenever the session is idle.
func (c *ClaudeSocket) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.kick:
		}
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 0 {
			continue
		}
		s, err := c.session()
		if err != nil || s.Status != "idle" {
			continue
		}
		c.mu.Lock()
		text := strings.Join(c.pending, "\n\n")
		c.mu.Unlock()
		if err := c.push(ctx, text); err != nil {
			fails++
			if fails == 3 {
				c.Log.Printf("cannot write to %s: %v", c.Socket, err)
			}
			continue
		}
		fails = 0
		c.mu.Lock()
		c.pending = c.pending[n:]
		c.mu.Unlock()
		// The session is busy with what it was just given; let its
		// registry catch up before looking again.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// push writes one user message, as newline-delimited JSON frames, the way
// Claude Code's own peers do.
func (c *ClaudeSocket) push(ctx context.Context, content string) error {
	var frames []byte
	if c.Token != "" {
		b, _ := json.Marshal(map[string]string{"type": "auth", "token": c.Token})
		frames = append(append(frames, b...), '\n')
	}
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": content}})
	frames = append(append(frames, b...), '\n')
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, "unix", c.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Write(frames)
	return err
}
