package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/louisiane/internal/client"
	"github.com/jrimmer/louisiane/internal/connector"
	"github.com/jrimmer/louisiane/internal/proto"
	"github.com/jrimmer/louisiane/internal/relay"
)

// syncBuf is a buffer two goroutines can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// stdout runs f and returns what it printed.
func stdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := f()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// agent is a stand-in agent process with a connector printing what the
// agent would be shown.
type agent struct {
	proc   *exec.Cmd
	out    *syncBuf
	member string
	c      *client.Client
}

func startAgent(t *testing.T, ctx context.Context, kind, dir string) *agent {
	t.Helper()
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.Process.Kill(); proc.Wait() })
	h, err := client.HarnessFor(proc.Process.Pid, kind)
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{proc: proc, out: &syncBuf{}}
	go connector.Run(ctx, connector.Options{Harness: h, Deliver: &connector.Printer{W: a.out}})
	sock := client.SocketPath(h.PID)
	eventually(t, "the connector's socket", func() bool { _, err := os.Stat(sock); return err == nil })
	var st connector.Status
	if err := localCall(sock, "/connect", connector.ConnectReq{Cwd: dir}, &st); err != nil || st.Hello == nil {
		t.Fatalf("connect: %v %+v", err, st)
	}
	a.member = st.Hello.Member
	cfg, err := client.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	a.c = client.New(cfg)
	a.c.Member = a.member
	return a
}

func (a *agent) send(t *testing.T, req proto.SendReq) proto.SendResp {
	t.Helper()
	var resp proto.SendResp
	if err := a.c.Do(context.Background(), "POST", "/v1/send", req, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMeshEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LAL_CONFIG", filepath.Join(dir, "host.json"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	state := filepath.Join(dir, "state")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	// The first name doesn't resolve, so joining has to fall through to the
	// address.
	r, err := relay.Open(relay.Config{StateDir: state, Names: []string{"relay.invalid", "127.0.0.1"}, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { r.Serve(ctx, net.JoinHostPort("127.0.0.1", itoa(port))); close(served) }()
	t.Cleanup(func() { cancel(); <-served; r.Close() })
	eventually(t, "the relay", func() bool { _, err := os.Stat(filepath.Join(state, "admin.sock")); return err == nil })

	inv := stdout(t, func() error { return cmdInvite([]string{"laptop", "--state-dir", state}) })
	token := ""
	for _, f := range strings.Fields(inv) {
		if strings.HasPrefix(f, tokenPrefix) {
			token = f
		}
	}
	if token == "" {
		t.Fatalf("no token in:\n%s", inv)
	}
	out := stdout(t, func() error { return cmdJoin([]string{token, "--no-install"}) })
	if !strings.Contains(out, "127.0.0.1") || !strings.Contains(out, "as laptop") {
		t.Fatalf("join: %s", out)
	}

	proj := filepath.Join(dir, "alpha")
	os.MkdirAll(proj, 0o755)
	a := startAgent(t, ctx, "claude", proj)
	b := startAgent(t, ctx, "omp", proj)
	if !strings.HasPrefix(a.member, "alpha/claude-") || !strings.HasPrefix(b.member, "alpha/omp-") {
		t.Fatalf("members %s %s", a.member, b.member)
	}

	q := a.send(t, proto.SendReq{To: "#alpha", Body: "who has the parser?", Ask: true})
	eventually(t, "the question", func() bool {
		return strings.Contains(b.out.String(), "[lal] question from agent "+a.member+" in #alpha") &&
			strings.Contains(b.out.String(), "│ who has the parser?")
	})
	b.send(t, proto.SendReq{ReplyTo: q.ID, Body: "I do"})
	eventually(t, "the reply", func() bool { return strings.Contains(a.out.String(), "│ I do") })
	if strings.Contains(a.out.String(), "who has the parser?") {
		t.Fatal("the asker heard its own question")
	}

	// lal init, twice: the second run changes nothing.
	wd, _ := os.Getwd()
	os.Chdir(proj)
	t.Cleanup(func() { os.Chdir(wd) })
	first := stdout(t, func() error { return cmdInit(nil) })
	second := stdout(t, func() error { return cmdInit(nil) })
	agents, _ := os.ReadFile(filepath.Join(proj, "AGENTS.md"))
	claude, _ := os.ReadFile(filepath.Join(proj, "CLAUDE.md"))
	if !strings.Contains(first, "Updated") || strings.Contains(second, "Updated") ||
		!strings.Contains(string(agents), "`lal prime`") || string(claude) != "@AGENTS.md\n" {
		t.Fatalf("init:\n%s\n%s\nAGENTS.md: %s\nCLAUDE.md: %q", first, second, agents, claude)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
