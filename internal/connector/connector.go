// Package connector keeps one agent session on the mesh. It holds the
// session's stream from the relay, puts each message into the session
// through a Deliverer, and listens on a local socket for `lal prime` to
// tell it which project to connect as.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/louisiane/internal/client"
	"github.com/jrimmer/louisiane/internal/proto"
)

// Deliverer puts text into the agent's conversation.
type Deliverer interface {
	// Name says how it delivers, for `lal prime`.
	Name() string
	// Deliver queues text for the session; it never blocks for long.
	Deliver(text string)
	// Connected tells the deliverer the session's member name.
	Connected(member string)
}

type Options struct {
	Harness client.Harness
	Deliver Deliverer
	Log     *log.Logger
}

// ConnectReq is what `lal prime` sends to the connector.
type ConnectReq struct {
	Cwd string `json:"cwd"`
	As  string `json:"as,omitempty"`
}

// Status is the connector's answer to prime.
type Status struct {
	Connected bool         `json:"connected"`
	Kind      string       `json:"kind"`
	Delivery  string       `json:"delivery"`
	Hello     *proto.Hello `json:"hello,omitempty"`
	Error     string       `json:"error,omitempty"`
}

type Connector struct {
	opt  Options
	cfg  *client.Config
	c    *client.Client
	sess client.Session

	mu      sync.Mutex
	proj    client.Project
	member  string
	rooms   []string
	lastID  string
	hello   *proto.Hello
	cancel  context.CancelFunc
	helloCh chan struct{}
	err     error
}

// Run serves until ctx ends.
func Run(ctx context.Context, opt Options) error {
	if opt.Log == nil {
		opt.Log = log.New(os.Stderr, "lal connector: ", log.LstdFlags)
	}
	cfg, err := client.LoadConfig()
	if err != nil {
		return err
	}
	k := &Connector{opt: opt, cfg: cfg, c: client.New(cfg)}
	sock := client.SocketPath(opt.Harness.PID)
	k.sess = client.Session{PID: opt.Harness.PID, ProcStart: opt.Harness.ProcStart, Kind: opt.Harness.Kind, Socket: sock}
	if err := os.MkdirAll(client.RuntimeDir()+"/sessions", 0o700); err != nil {
		return err
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)
	if err := k.sess.Save(); err != nil {
		ln.Close()
		return err
	}
	defer client.RemoveSession(opt.Harness.PID)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /connect", k.handleConnect)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(k.status())
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	// A connector lives as long as its agent.
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-t.C:
			if !opt.Harness.Alive() {
				opt.Log.Printf("agent process %d is gone", opt.Harness.PID)
				break wait
			}
		}
	}
	k.mu.Lock()
	if k.cancel != nil {
		k.cancel()
	}
	me := k.member
	k.mu.Unlock()
	if me != "" {
		// Leave at once rather than after the relay's grace.
		bye := *k.c
		bye.Member = me
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		bye.Do(bctx, http.MethodPost, "/v1/bye", nil, nil)
		cancel()
	}
	sctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	return nil
}

func (k *Connector) status() Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	s := Status{Connected: k.hello != nil && k.cancel != nil, Kind: k.opt.Harness.Kind, Delivery: k.opt.Deliver.Name(), Hello: k.hello}
	if k.err != nil {
		s.Error = k.err.Error()
	}
	return s
}

func (k *Connector) handleConnect(w http.ResponseWriter, r *http.Request) {
	var req ConnectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	st := k.connect(req)
	json.NewEncoder(w).Encode(st)
}

// connect starts (or restarts, for another project) the stream and waits
// for its hello.
func (k *Connector) connect(req ConnectReq) Status {
	p, err := client.DetectProject(req.Cwd, k.cfg.Host)
	if err != nil {
		return Status{Error: err.Error(), Kind: k.opt.Harness.Kind, Delivery: k.opt.Deliver.Name()}
	}
	if req.As != "" {
		p.Name = req.As
	}
	k.mu.Lock()
	if k.cancel != nil && k.proj == p {
		k.mu.Unlock()
		return k.waitHello()
	}
	if k.cancel != nil {
		k.cancel()
		k.member, k.rooms, k.lastID = "", nil, ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	k.proj, k.cancel, k.hello, k.err = p, cancel, nil, nil
	k.helloCh = make(chan struct{})
	k.mu.Unlock()
	go k.loop(ctx)
	return k.waitHello()
}

func (k *Connector) waitHello() Status {
	k.mu.Lock()
	ch := k.helloCh
	k.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
	}
	return k.status()
}

// loop holds the stream, reconnecting after a short pause when it drops.
func (k *Connector) loop(ctx context.Context) {
	delay := 500 * time.Millisecond
	for ctx.Err() == nil {
		start := time.Now()
		err := k.c.Stream(ctx, k.streamPath(), k.last(), k.handle)
		if ctx.Err() != nil {
			return
		}
		k.mu.Lock()
		k.err = err
		ch := k.helloCh
		k.mu.Unlock()
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Status/100 == 4 && ae.Status != 429 {
			// The relay refused this connection; prime will show why.
			select {
			case <-ch:
			default:
				close(ch)
			}
			k.opt.Log.Printf("refused: %v", err)
			delay = 30 * time.Second
		} else {
			k.opt.Log.Printf("stream: %v", err)
		}
		if time.Since(start) > time.Minute {
			delay = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

func (k *Connector) last() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lastID
}

func (k *Connector) streamPath() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	q := url.Values{}
	q.Set("key", k.proj.Key)
	q.Set("name", k.proj.Name)
	q.Set("root", k.proj.Root)
	q.Set("kind", k.opt.Harness.Kind)
	q.Set("pid", strconv.Itoa(k.opt.Harness.PID))
	if k.member != "" {
		q.Set("member", k.member)
	}
	var topics []string
	for _, r := range k.rooms {
		if r != proto.AllRoom && r != k.proj.Name && !strings.Contains(r, "/") {
			topics = append(topics, r)
		}
	}
	if len(topics) > 0 {
		q.Set("rooms", strings.Join(topics, ","))
	}
	return "/v1/stream?" + q.Encode()
}

func (k *Connector) handle(id string, ev proto.Event) error {
	k.mu.Lock()
	if id != "" {
		k.lastID = id
	}
	me := k.member
	k.mu.Unlock()
	switch ev.Type {
	case proto.EventHello:
		h := ev.Hello
		k.mu.Lock()
		first := k.hello == nil
		k.member, k.hello, k.rooms, k.err = h.Member, h, h.Rooms, nil
		k.sess.Member, k.sess.Project = h.Member, h.Project.Name
		sess := k.sess
		ch := k.helloCh
		k.mu.Unlock()
		if err := sess.Save(); err != nil {
			k.opt.Log.Printf("session file: %v", err)
		}
		k.opt.Deliver.Connected(h.Member)
		select {
		case <-ch:
		default:
			close(ch)
		}
		if !first && me != "" && me != h.Member {
			k.opt.Deliver.Deliver(fmt.Sprintf("[lal] The relay restarted and this session is now %s on the mesh.", h.Member))
		}
		return nil
	case proto.EventRoom:
		k.mu.Lock()
		if ev.Action == "join" && !slices.Contains(k.rooms, ev.Room) {
			k.rooms = append(k.rooms, ev.Room)
		} else if ev.Action == "leave" {
			k.rooms = slices.DeleteFunc(k.rooms, func(r string) bool { return r == ev.Room })
		}
		k.mu.Unlock()
		return nil
	}
	if text := Format(ev, me); text != "" {
		k.opt.Deliver.Deliver(text)
	}
	return nil
}
