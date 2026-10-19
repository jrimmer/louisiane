package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Project is what a directory says about the project it belongs to.
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Root string `json:"root"`
}

// DetectProject works out the project for dir: the main worktree of its git
// repository (a linked worktree maps to the repo it came from), keyed by
// the origin remote when there is one.
func DetectProject(dir, host string) (Project, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, err
	}
	root := dir
	if common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		if filepath.Base(common) == ".git" {
			root = filepath.Dir(common)
		} else if top, err := git(dir, "rev-parse", "--show-toplevel"); err == nil {
			root = top // a bare repo's worktree
		}
	}
	p := Project{Root: root, Name: filepath.Base(root), Key: host + ":" + root}
	if remote, err := git(dir, "remote", "get-url", "origin"); err == nil && remote != "" {
		if k := NormalizeRemote(remote); k != "" {
			p.Key = k
			p.Name = filepath.Base(k)
		}
	}
	return p, nil
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// NormalizeRemote turns any spelling of a git remote into host/path:
// git@github.com:jrimmer/lal.git and https://github.com/jrimmer/lal
// both become github.com/jrimmer/lal.
func NormalizeRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if u, err := url.Parse(r); err == nil && u.Host != "" {
		r = u.Hostname() + "/" + strings.TrimPrefix(u.Path, "/")
	} else if at := strings.Index(r, "@"); at >= 0 && strings.Contains(r[at:], ":") {
		r = strings.Replace(r[at+1:], ":", "/", 1)
	} else if !strings.Contains(r, "://") && strings.HasPrefix(r, "/") {
		return "" // a local path: not a shared identity
	}
	r = strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
	return strings.ToLower(r)
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "env": true,
	"timeout": true, "nohup": true, "script": true, "sudo": true, "xargs": true, "nice": true, "stdbuf": true,
	"lal": true}

// Harness is the agent process a command runs under.
type Harness struct {
	PID       int
	ProcStart string
	Kind      string
}

// FindHarness walks up from this process to the agent that started it: the
// first ancestor that is not a shell, then up through parents of the same
// name (an agent's own worker processes). $CLAUDE_PID wins when set.
func FindHarness() (Harness, error) {
	if s := os.Getenv("CLAUDE_PID"); s != "" {
		if pid, err := strconv.Atoi(s); err == nil {
			if st, err := procStart(pid); err == nil {
				return Harness{PID: pid, ProcStart: st, Kind: "claude"}, nil
			}
		}
	}
	pid := os.Getppid()
	for pid > 1 {
		name, ppid, err := procInfo(pid)
		if err != nil {
			return Harness{}, err
		}
		if !shells[name] {
			for ppid > 1 {
				pname, pp, err := procInfo(ppid)
				if err != nil || pname != name {
					break
				}
				pid, ppid = ppid, pp
			}
			st, err := procStart(pid)
			if err != nil {
				return Harness{}, err
			}
			return Harness{PID: pid, ProcStart: st, Kind: KindOf(name)}, nil
		}
		pid = ppid
	}
	return Harness{}, errors.New("no agent process above this command")
}

// KindOf names an agent from its process name.
func KindOf(comm string) string {
	c := strings.ToLower(comm)
	for _, k := range []string{"claude", "omp", "pi", "codex", "gemini", "hermes", "openclaw", "opencode", "cursor"} {
		if c == k || strings.HasPrefix(c, k+"-") {
			return k
		}
	}
	return c
}

// procInfo reads a process's name and parent from /proc.
func procInfo(pid int) (string, int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, err
	}
	s := string(b)
	open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || close < open {
		return "", 0, fmt.Errorf("bad /proc/%d/stat", pid)
	}
	f := strings.Fields(s[close+1:])
	if len(f) < 2 {
		return "", 0, fmt.Errorf("bad /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(f[1])
	return s[open+1 : close], ppid, err
}

// procStart is a process's start time in clock ticks since boot, which with
// its pid names it uniquely.
func procStart(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 20 {
		return "", fmt.Errorf("bad /proc/%d/stat", pid)
	}
	return f[19], nil
}

// Session is what a connector records for the CLI commands its agent runs.
type Session struct {
	PID       int    `json:"pid"`
	ProcStart string `json:"proc_start"`
	Kind      string `json:"kind"`
	Socket    string `json:"socket"`
	Member    string `json:"member,omitempty"`
	Project   string `json:"project,omitempty"`
}

// RuntimeDir is where session files and connector sockets live.
func RuntimeDir() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("louisiane-%d", os.Getuid()))
	}
	return filepath.Join(base, "louisiane")
}

func sessionPath(pid int) string {
	return filepath.Join(RuntimeDir(), "sessions", strconv.Itoa(pid)+".json")
}

// SocketPath is the connector socket for an agent process.
func SocketPath(pid int) string {
	return filepath.Join(RuntimeDir(), "sessions", strconv.Itoa(pid)+".sock")
}

func (s *Session) Save() error {
	p := sessionPath(s.PID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func RemoveSession(pid int) {
	os.Remove(sessionPath(pid))
	os.Remove(SocketPath(pid))
}

// LoadSession reads the session for h, if its connector wrote one for this
// very process.
func LoadSession(h Harness) (*Session, error) {
	b, err := os.ReadFile(sessionPath(h.PID))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.ProcStart != h.ProcStart {
		return nil, os.ErrNotExist // an older process with the same pid
	}
	return &s, nil
}

// HarnessFor describes the agent process pid, as its own extension names it.
func HarnessFor(pid int, kind string) (Harness, error) {
	st, err := procStart(pid)
	if err != nil {
		return Harness{}, err
	}
	if kind == "" {
		name, _, err := procInfo(pid)
		if err != nil {
			return Harness{}, err
		}
		kind = KindOf(name)
	}
	return Harness{PID: pid, ProcStart: st, Kind: kind}, nil
}

// Alive reports whether the agent process h is still running.
func (h Harness) Alive() bool {
	st, err := procStart(h.PID)
	return err == nil && st == h.ProcStart
}
