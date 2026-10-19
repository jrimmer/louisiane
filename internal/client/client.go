// Package client is the relay's client side: the host config, an HTTP
// client pinned to the relay's certificate, and stream reading.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jrimmer/louisiane/internal/pin"
	"github.com/jrimmer/louisiane/internal/proto"
)

const (
	MemberHeader = "X-Lal-Member"
	AsHostHeader = "X-Lal-As-Host"
)

// Config is a joined host's relay address, pin and token.
type Config struct {
	Relay string `json:"relay"`
	Pin   string `json:"pin"`
	Host  string `json:"host"`
	Token string `json:"token"`
}

// ConfigPath is $LAL_CONFIG or ~/.config/louisiane/host.json.
func ConfigPath() string {
	if p := os.Getenv("LAL_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "louisiane", "host.json")
}

var ErrNotJoined = errors.New("this host has not joined a relay; run `lal join CODE --relay URL --pin PIN` (from `lal invite` on the relay host)")

func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotJoined
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	if r := os.Getenv("LAL_RELAY"); r != "" {
		c.Relay = r
	}
	return &c, nil
}

func (c *Config) Save() error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// Client talks to the relay as a host, optionally speaking for a member.
type Client struct {
	base   string
	token  string
	Member string
	AsHost bool
	// User and Password, when set, add the dashboard's login: the owner's.
	User, Password string
	http           *http.Client // calls
	stream         *http.Client // long-lived streams: no overall timeout
}

// New makes a client for a joined host.
func New(c *Config) *Client {
	t := pin.Transport(c.Pin)
	return &Client{base: strings.TrimRight(c.Relay, "/"), token: c.Token,
		http: &http.Client{Transport: t, Timeout: 30 * time.Second}, stream: &http.Client{Transport: t}}
}

// NewPinned makes an unauthenticated client, for joining.
func NewPinned(relay, fp string) *Client {
	return New(&Config{Relay: relay, Pin: fp})
}

// NewAdmin talks to the relay over its admin socket.
func NewAdmin(sock string) *Client {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &Client{base: "http://relay", http: &http.Client{Transport: t, Timeout: 30 * time.Second}, stream: &http.Client{Transport: t}}
}

// APIError is an error the relay answered with.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	if c.AsHost {
		req.Header.Set(AsHostHeader, "1")
	} else if c.Member != "" {
		req.Header.Set(MemberHeader, c.Member)
	}
	return req, nil
}

func decodeErr(resp *http.Response) error {
	var e proto.ErrorResp
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return &APIError{Status: resp.StatusCode, Msg: e.Error}
	}
	return &APIError{Status: resp.StatusCode, Msg: fmt.Sprintf("relay answered %s: %s", resp.Status, strings.TrimSpace(string(b)))}
}

// Do makes one call and decodes the answer into out.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("relay %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeErr(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream opens an SSE stream and calls fn for each event until the stream
// ends, ctx ends or fn returns an error. lastID resumes a stream.
func (c *Client) Stream(ctx context.Context, path, lastID string, fn func(id string, ev proto.Event) error) error {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return fmt.Errorf("relay %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeErr(resp)
	}
	return ReadSSE(resp.Body, fn)
}

// ReadSSE parses a stream of "id:" and "data:" lines.
func ReadSSE(r io.Reader, fn func(id string, ev proto.Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var id string
	var data []byte
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				var ev proto.Event
				if err := json.Unmarshal(data, &ev); err == nil {
					if err := fn(id, ev); err != nil {
						return err
					}
				}
			}
			id, data = "", nil
		case strings.HasPrefix(line, "id: "):
			id = line[4:]
		case strings.HasPrefix(line, "data: "):
			data = append(data, line[6:]...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
