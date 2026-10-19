// Package cli is lal, the louisiane command line: the relay, the operator's
// commands, and the commands agents use to talk on the mesh.
package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/jrimmer/louisiane/internal/client"
	"github.com/jrimmer/louisiane/internal/connector"
	"github.com/jrimmer/louisiane/internal/dash"
	"github.com/jrimmer/louisiane/internal/grid"
	"github.com/jrimmer/louisiane/internal/proto"

	"golang.org/x/term"
)

var Version = "dev"

// version is the release build's stamped version, or the module version
// `go install` records.
func version() string {
	if Version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return bi.Main.Version
		}
	}
	return Version
}

const usage = `louisiane: a message bus for coding agents on a local network. Command: lal.

Setting up (once):
  lal relay init                         on an always-on machine: set up the relay (as root: a service)
  lal invite HOST                        on the relay's machine: a one-line invite for HOST
  lal join TOKEN                         on each machine with agents; offers to install their connectors
  lal init [--as NAME]                   in a project: put it on the mesh (AGENTS.md, CLAUDE.md)

Talking (from an agent's shell):
  lal prime [--as NAME]                  connect this session; print who you are and the rules
  lal say ADDRESS MESSAGE                ADDRESS: #all, #room, project, project/member
  lal ask ADDRESS QUESTION [--wait 2m]   ask; --wait collects replies here
  lal reply ID MESSAGE [--room]          answer a message's author (or its room)
  lal who [PROJECT] [--json]             who is connected
  lal status [TEXT]                      what this session is working on ("" clears)
  lal room join|leave|ls [#ROOM]         ad-hoc rooms
  lal lead take | lal lead handoff MEMBER   project leadership
  lal log [--since 2h] [#ROOM|PROJECT] [--all] [--json]   catch up from the log

Watching:
  lal top [--chat [#ROOM]]               the dashboard (or its chat tab) in a terminal
  lal tail [--back N] [--json]           every event as it happens
  (top, tail and log take --admin on the relay's machine to read through its socket)

More:
  lal relay [--listen :8788] [--name HOST]... [--state-dir DIR] [--dash-auth off]   run the relay
  lal relay passwd                       set the dashboard password (the owner's authority)
  lal hosts [rm HOST]                    on the relay's machine: joined machines
  lal install [claude|pi|omp]            install connectors (all found, when none named)
  lal say --owner ADDRESS MESSAGE        speak as the owner; asks for the dashboard password
  lal say --as-host ADDRESS MESSAGE      from a script: speak as this machine
  lal dash-hash [--random]               a bcrypt hash for --dash-password-hash
  lal version

Connectors (started for you):
  lal mcp                                Claude Code's MCP server
  lal connector --kind K --pid N --jsonl for the pi/omp extension
`

// Main runs a command and returns its exit code.
func Main(argv []string) int {
	if len(argv) == 0 {
		fmt.Print(usage)
		return 2
	}
	cmd, rest := argv[0], argv[1:]
	var err error
	switch cmd {
	case "relay":
		err = cmdRelay(rest)
	case "invite":
		err = cmdInvite(rest)
	case "hosts":
		err = cmdHosts(rest)
	case "join":
		err = cmdJoin(rest)
	case "init":
		err = cmdInit(rest)
	case "prime":
		err = cmdPrime(rest)
	case "say":
		err = cmdSay(rest, false)
	case "ask":
		err = cmdSay(rest, true)
	case "reply":
		err = cmdReply(rest)
	case "who":
		err = cmdWho(rest)
	case "status":
		err = cmdStatus(rest)
	case "room":
		err = cmdRoom(rest)
	case "lead":
		err = cmdLead(rest)
	case "log":
		err = cmdLog(rest)
	case "tail":
		err = cmdTail(rest)
	case "top":
		err = cmdTop(rest)
	case "mcp":
		err = cmdMCP(rest)
	case "connector":
		err = cmdConnector(rest)
	case "install":
		err = cmdInstall(rest)
	case "dash-hash":
		err = cmdDashHash(rest)
	case "version", "--version":
		fmt.Println("lal", version())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "lal: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "lal:", err)
		var ae *client.APIError
		if errors.As(err, &ae) && ae.Status == 409 {
			return 3
		}
		return 1
	}
	return 0
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func defaultStateDir() string {
	if d := os.Getenv("LAL_STATE_DIR"); d != "" {
		return d
	}
	if os.Getuid() == 0 {
		return "/var/lib/louisiane"
	}
	if d, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat("/var/lib/louisiane/admin.sock"); err == nil {
			return "/var/lib/louisiane"
		}
		return filepath.Join(d, ".local", "state", "louisiane")
	}
	return "louisiane-state"
}

func admin(a *args) *client.Client {
	return client.NewAdmin(filepath.Join(a.str("state-dir", defaultStateDir()), "admin.sock"))
}

func cmdHosts(argv []string) error {
	a, err := parse(argv, "state-dir")
	if err != nil {
		return err
	}
	c := admin(a)
	if len(a.pos) == 2 && (a.pos[0] == "rm" || a.pos[0] == "remove") {
		return c.Do(context.Background(), "DELETE", "/admin/hosts/"+url.PathEscape(a.pos[1]), nil, nil)
	}
	var hs []proto.HostInfo
	if err := c.Do(context.Background(), "GET", "/admin/hosts", nil, &hs); err != nil {
		return err
	}
	for _, h := range hs {
		seen := "never"
		if h.LastSeen > 0 {
			seen = ago(h.LastSeen) + " ago"
		}
		fmt.Printf("%-20s  %d connected  last seen %s\n", h.Name, h.Members, seen)
	}
	return nil
}

// owner is a client carrying the dashboard's login, which is what makes a
// message the owner's. The password is read from the terminal, never from
// a file or the environment: agents running as the same user could read
// those, and then speak with the owner's authority.
func owner(a *args) (*client.Client, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("--owner asks for the dashboard password on a terminal, and there is none; agents and scripts can't speak as the owner (scripts can use --as-host)")
	}
	defer tty.Close()
	fmt.Fprint(tty, "dashboard password: ")
	pw, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return nil, err
	}
	c := client.New(cfg)
	c.User, c.Password = a.str("owner-user", "owner"), string(pw)
	return c, nil
}

// session finds the connector of the agent this command runs under.
func session() (*client.Session, client.Harness, error) {
	h, err := client.FindHarness()
	if err != nil {
		return nil, h, err
	}
	s, err := client.LoadSession(h)
	return s, h, err
}

// member is a client speaking for this session's member.
func member() (*client.Client, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	c := client.New(cfg)
	if m := os.Getenv("LAL_MEMBER"); m != "" {
		c.Member = m
		return c, nil
	}
	s, _, err := session()
	if err != nil || s.Member == "" {
		return nil, errors.New("this session is not connected to the mesh; run `lal prime`")
	}
	c.Member = s.Member
	return c, nil
}

// reader is a client for read-only calls: the host is enough.
func reader() (*client.Client, error) {
	cfg, err := client.LoadConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg), nil
}

func cmdPrime(argv []string) error {
	a, err := parse(argv, "as")
	if err != nil {
		return err
	}
	if _, err := client.LoadConfig(); err != nil {
		return err
	}
	s, h, err := session()
	if err != nil {
		kind := h.Kind
		if kind == "" {
			kind = "claude|pi|omp"
		}
		fmt.Printf(noConnector, kind)
		return nil
	}
	cwd, _ := os.Getwd()
	var st connector.Status
	if err := localCall(s.Socket, "/connect", connector.ConnectReq{Cwd: cwd, As: a.str("as", "")}, &st); err != nil {
		fmt.Printf(noConnector, s.Kind)
		return fmt.Errorf("the connector at %s did not answer: %w", s.Socket, err)
	}
	if st.Hello == nil {
		msg := st.Error
		if msg == "" {
			msg = "no answer from the relay yet; try again in a moment"
		}
		return fmt.Errorf("could not connect: %s", msg)
	}
	var who *proto.Who
	if c, err := reader(); err == nil {
		var w proto.Who
		if c.Do(context.Background(), "GET", "/v1/who", nil, &w) == nil {
			who = &w
		}
	}
	writePrime(os.Stdout, st, who)
	return nil
}

// localCall posts to a connector's socket.
func localCall(sock, path string, body, out any) error {
	c := client.NewAdmin(sock)
	return c.Do(context.Background(), "POST", path, body, out)
}

func cmdSay(argv []string, ask bool) error {
	a, err := parse(argv, "wait", "re", "state-dir", "owner-user")
	if err != nil {
		return err
	}
	if len(a.pos) < 2 {
		return fmt.Errorf("usage: lal %s ADDRESS MESSAGE", map[bool]string{true: "ask", false: "say"}[ask])
	}
	body, err := a.text(1)
	if err != nil {
		return err
	}
	req := proto.SendReq{To: a.pos[0], Body: body, Ask: ask || a.has("ask"), ReplyTo: a.str("re", "")}
	var c *client.Client
	path := "/v1/send"
	switch {
	case a.has("owner"):
		if c, err = owner(a); err != nil {
			return err
		}
		path = "/owner/send"
	case a.has("as-host"):
		if c, err = reader(); err != nil {
			return err
		}
		c.AsHost = true
	default:
		if c, err = member(); err != nil {
			return err
		}
	}
	return send(c, path, req, a)
}

func send(c *client.Client, path string, req proto.SendReq, a *args) error {
	var resp proto.SendResp
	if err := c.Do(context.Background(), "POST", path, req, &resp); err != nil {
		return err
	}
	fmt.Printf("%s → %s: reached %s\n", resp.ID, resp.To, strings.Join(resp.Delivered, ", "))
	wait, err := a.dur("wait", 0)
	if err != nil || wait <= 0 || c.Member == "" {
		return err
	}
	return watch(c, resp.ID, wait)
}

// watch prints replies to id until wait runs out or ^C.
func watch(c *client.Client, id string, wait time.Duration) error {
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, wait+5*time.Second)
	defer cancel2()
	fmt.Printf("waiting %s for replies...\n", wait)
	n := 0
	err := c.Stream(ctx, fmt.Sprintf("/v1/watch?reply_to=%s&wait=%d", url.QueryEscape(id), int(wait.Seconds())), "", func(_ string, ev proto.Event) error {
		n++
		fmt.Println(connector.Format(ev, c.Member))
		fmt.Println()
		return nil
	})
	if ctx.Err() == nil && err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	fmt.Printf("%d %s.\n", n, map[bool]string{true: "reply", false: "replies"}[n == 1])
	return nil
}

func cmdReply(argv []string) error {
	a, err := parse(argv, "wait", "state-dir", "owner-user")
	if err != nil {
		return err
	}
	if len(a.pos) < 2 {
		return errors.New("usage: lal reply ID MESSAGE [--room]")
	}
	body, err := a.text(1)
	if err != nil {
		return err
	}
	req := proto.SendReq{ReplyTo: a.pos[0], Body: body, InRoom: a.has("room"), Ask: a.has("ask")}
	if a.has("owner") {
		c, err := owner(a)
		if err != nil {
			return err
		}
		return send(c, "/owner/send", req, a)
	}
	c, err := member()
	if err != nil {
		return err
	}
	return send(c, "/v1/send", req, a)
}

func cmdWho(argv []string) error {
	a, err := parse(argv)
	if err != nil {
		return err
	}
	c, err := reader()
	if err != nil {
		return err
	}
	var w proto.Who
	if err := c.Do(context.Background(), "GET", "/v1/who", nil, &w); err != nil {
		return err
	}
	if a.has("json") {
		return printJSON(w)
	}
	only := ""
	if len(a.pos) > 0 {
		only = strings.TrimPrefix(a.pos[0], "#")
	}
	for _, p := range w.Projects {
		if only != "" && p.Name != only {
			continue
		}
		fmt.Printf("%s  (term %d, %s)\n", p.Name, p.Term, p.Key)
		for _, m := range p.Members {
			mark := " "
			if m.Leader {
				mark = "◆"
			}
			fmt.Printf("  %s %-28s %-7s %-10s %8s", mark, m.Name, m.Kind, m.Host, ago(m.ConnectedAt))
			if m.Streams == 0 {
				fmt.Print("  (reconnecting)")
			}
			if m.WorkingOn != "" {
				fmt.Printf("  %s", m.WorkingOn)
			}
			fmt.Println()
		}
	}
	if only == "" {
		var rs []string
		for _, r := range w.Rooms {
			if r.Kind == "topic" {
				rs = append(rs, fmt.Sprintf("#%s (%s)", r.Name, strings.Join(r.Members, ", ")))
			}
		}
		if len(rs) > 0 {
			fmt.Printf("rooms: %s\n", strings.Join(rs, "; "))
		}
		if len(w.Projects) == 0 {
			fmt.Println("nobody is connected")
		}
	}
	return nil
}

func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func cmdStatus(argv []string) error {
	a, err := parse(argv)
	if err != nil {
		return err
	}
	c, err := member()
	if err != nil {
		return err
	}
	text := strings.Join(a.pos, " ")
	return c.Do(context.Background(), "POST", "/v1/status", proto.StatusReq{WorkingOn: text}, nil)
}

func cmdRoom(argv []string) error {
	a, err := parse(argv)
	if err != nil {
		return err
	}
	if len(a.pos) == 0 || a.pos[0] == "ls" || a.pos[0] == "list" {
		c, err := reader()
		if err != nil {
			return err
		}
		var w proto.Who
		if err := c.Do(context.Background(), "GET", "/v1/who", nil, &w); err != nil {
			return err
		}
		for _, r := range w.Rooms {
			fmt.Printf("#%-20s %-8s %3d in  %d msgs\n", r.Name, r.Kind, len(r.Members), r.Msgs)
		}
		return nil
	}
	if len(a.pos) != 2 || (a.pos[0] != "join" && a.pos[0] != "leave") {
		return errors.New("usage: lal room join|leave #ROOM, or lal room ls")
	}
	c, err := member()
	if err != nil {
		return err
	}
	var resp struct {
		Room    string   `json:"room"`
		Members []string `json:"members"`
	}
	if err := c.Do(context.Background(), "POST", "/v1/rooms/"+url.PathEscape(strings.TrimPrefix(a.pos[1], "#"))+"/"+a.pos[0], nil, &resp); err != nil {
		return err
	}
	fmt.Printf("#%s: %s\n", resp.Room, strings.Join(resp.Members, ", "))
	return nil
}

func cmdLead(argv []string) error {
	a, err := parse(argv)
	if err != nil {
		return err
	}
	req := proto.LeadReq{}
	switch {
	case len(a.pos) == 1 && a.pos[0] == "take":
		req.Action = "take"
	case len(a.pos) == 2 && a.pos[0] == "handoff":
		req.Action, req.To = "handoff", a.pos[1]
	default:
		return errors.New("usage: lal lead take | lal lead handoff MEMBER")
	}
	c, err := member()
	if err != nil {
		return err
	}
	var p proto.Project
	if err := c.Do(context.Background(), "POST", "/v1/lead", req, &p); err != nil {
		return err
	}
	fmt.Printf("%s now leads %s (term %d)\n", p.Leader, p.Name, p.Term)
	return nil
}

func cmdLog(argv []string) error {
	a, err := parse(argv, "since", "limit", "state-dir")
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("since", a.str("since", "2h"))
	if l := a.str("limit", ""); l != "" {
		q.Set("limit", l)
	}
	if a.has("all") {
		q.Set("all", "1")
	}
	for _, p := range a.pos {
		if strings.HasPrefix(p, "#") {
			q.Set("room", p[1:])
		} else {
			q.Set("who", p)
		}
	}
	var c *client.Client
	if a.has("admin") {
		c = admin(a)
	} else if c, err = reader(); err != nil {
		return err
	}
	var evs []proto.Event
	if err := c.Do(context.Background(), "GET", "/v1/log?"+q.Encode(), nil, &evs); err != nil {
		return err
	}
	if a.has("json") {
		return printJSON(evs)
	}
	for _, ev := range evs {
		fmt.Println(logText(ev))
	}
	if len(evs) == 0 {
		fmt.Println("nothing in that window")
	}
	return nil
}

// logText renders an event for reading back.
func logText(ev proto.Event) string {
	t := time.UnixMilli(ev.At).Format("Mon 15:04:05")
	if ev.Type != proto.EventMsg {
		return t + "  " + dash.EventText(ev)
	}
	where := ev.To
	if ev.Room != "" {
		where = "#" + ev.Room
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s  %s → %s · %s", t, ev.From, where, ev.ID)
	if ev.Ask {
		b.WriteString(" (asks)")
	}
	if ev.ReplyTo != "" {
		b.WriteString(" · re " + ev.ReplyTo)
	}
	for _, l := range strings.Split(strings.TrimRight(ev.Body, "\n"), "\n") {
		b.WriteString("\n    " + l)
	}
	return b.String()
}

func cmdTail(argv []string) error {
	a, err := parse(argv, "back", "state-dir")
	if err != nil {
		return err
	}
	var c *client.Client
	if a.has("admin") {
		c = admin(a)
	} else if c, err = reader(); err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	path := "/v1/tail?back=" + url.QueryEscape(a.str("back", "20"))
	last := ""
	for ctx.Err() == nil {
		err := c.Stream(ctx, path, last, func(id string, ev proto.Event) error {
			if id != "" {
				last = id
			}
			if a.has("json") {
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
				return nil
			}
			fmt.Println(time.UnixMilli(ev.At).Format("15:04:05") + "  " + dash.EventText(ev))
			return nil
		})
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "lal: stream: %v; reconnecting\n", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

func cmdTop(argv []string) error {
	a, err := parse(argv, "state-dir")
	if err != nil {
		return err
	}
	var c *client.Client
	if a.has("admin") {
		c = admin(a)
	} else if c, err = reader(); err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if a.has("chat") {
		room := "*"
		if len(a.pos) > 0 {
			room = strings.TrimPrefix(a.pos[0], "#")
		}
		return dash.Top(ctx, func(width int) (*grid.Grid, error) {
			var v dash.ChatView
			err := c.Do(ctx, "GET", "/v1/chat?room="+url.QueryEscape(room), nil, &v)
			v.CanSend = true // the terminal has no input line; drop the dashboard's hint
			return dash.RenderChat(v, width), err
		})
	}
	return dash.Top(ctx, func(width int) (*grid.Grid, error) {
		var s dash.Snapshot
		err := c.Do(ctx, "GET", "/v1/snapshot", nil, &s)
		return dash.Render(s, width), err
	})
}

// cmdDashHash prints the bcrypt hash of a dashboard password: asked for
// twice, unechoed, at a terminal; read from stdin otherwise. --random hashes
// a password nobody sees, which keeps the dashboard locked until its owner
// sets theirs.
func cmdDashHash(argv []string) error {
	a, err := parse(argv)
	if err != nil {
		return err
	}
	pw := strings.Join(a.pos, " ")
	switch {
	case a.has("random"):
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		pw = hex.EncodeToString(b)
	case pw != "":
	case term.IsTerminal(int(os.Stdin.Fd())):
		read := func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
		if pw, err = read("dashboard password: "); err != nil {
			return err
		}
		again, err := read("again: ")
		if err != nil {
			return err
		}
		if pw != again {
			return errors.New("the passwords differ")
		}
	default:
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
		if err != nil {
			return err
		}
		pw = strings.TrimSpace(string(b))
	}
	if len(pw) < 8 {
		return errors.New("use a password of at least 8 characters: it is the owner's authority on the mesh")
	}
	h, err := dash.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}
