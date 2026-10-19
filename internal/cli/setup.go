package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/jrimmer/louisiane/internal/client"
	"github.com/jrimmer/louisiane/internal/dash"
	"github.com/jrimmer/louisiane/internal/pin"
	"github.com/jrimmer/louisiane/internal/proto"
	"github.com/jrimmer/louisiane/internal/relay"
)

// The relay: `lal relay` serves; `lal relay init` sets one up; `lal relay
// passwd` sets the dashboard password.
func cmdRelay(argv []string) error {
	if len(argv) > 0 {
		switch argv[0] {
		case "init":
			return relayInit(argv[1:])
		case "passwd", "password":
			return relayPasswd(argv[1:])
		}
	}
	a, err := parse(argv, "listen", "name", "state-dir", "dash-auth", "dash-user", "dash-password-hash")
	if err != nil {
		return err
	}
	dir := a.str("state-dir", defaultStateDir())
	set, err := relay.LoadSettings(dir)
	if err != nil {
		return err
	}
	cfg := relay.Config{StateDir: dir, Names: a.all("name")}
	if len(cfg.Names) == 0 {
		cfg.Names = set.Names
	}
	if len(cfg.Names) == 0 {
		cfg.Names = detectNames()
	}
	listen := a.str("listen", set.Listen)
	if listen == "" {
		listen = ":8788"
	}
	if _, port, err := splitPort(listen); err == nil {
		cfg.Port = port
	}
	switch a.str("dash-auth", "on") {
	case "off", "0", "false", "no":
	default:
		cfg.DashAuth = true
		cfg.DashUser = a.str("dash-user", set.DashUser)
		if cfg.DashUser == "" {
			cfg.DashUser = "owner"
		}
		cfg.DashHash = a.str("dash-password-hash", os.Getenv("LAL_DASH_PASSWORD_HASH"))
		if f := os.Getenv("LAL_DASH_PASSWORD_HASH_FILE"); cfg.DashHash == "" && f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			cfg.DashHash = strings.TrimSpace(string(b))
		}
		if cfg.DashHash == "" && !relay.HasDashHash(dir) {
			fmt.Fprintln(os.Stderr, "lal relay: no dashboard password yet, so the dashboard is locked; set one with `lal relay passwd`")
		}
	}
	r, err := relay.Open(cfg)
	if err != nil {
		return err
	}
	defer r.Close()
	ctx, cancel := signalContext()
	defer cancel()
	return r.Serve(ctx, listen)
}

func splitPort(addr string) (string, int, error) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(p)
	return host, port, err
}

// detectNames lists the names this machine is reached by: its full name,
// its short name, and its LAN addresses. Join tries them in order.
func detectNames() []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSuffix(strings.TrimSpace(s), ".")
		if s != "" && s != "localhost" && !contains(out, s) {
			out = append(out, s)
		}
	}
	if b, err := exec.Command("hostname", "-f").Output(); err == nil && strings.Contains(string(b), ".") {
		add(string(b))
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		if n := i.Name; strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth") ||
			strings.HasPrefix(n, "virbr") || strings.HasPrefix(n, "tailscale") || strings.HasPrefix(n, "wg") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() {
				add(ip.String())
			}
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

const relayUnit = `[Unit]
Description=louisiane relay: the message bus for coding agents on this network
After=network-online.target
Wants=network-online.target

[Service]
User=louisiane
Group=louisiane
ExecStart=/usr/local/bin/lal relay --state-dir /var/lib/louisiane
Restart=always
RestartSec=2
StateDirectory=louisiane
StateDirectoryMode=0700
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`

// relayInit sets up a relay on this machine: its settings, its
// certificate, the dashboard password and, as root with systemd, a service
// running as its own user.
func relayInit(argv []string) error {
	a, err := parse(argv, "state-dir", "name", "listen")
	if err != nil {
		return err
	}
	root := os.Geteuid() == 0
	service := root && !a.has("no-systemd") && haveSystemd()
	dir := a.str("state-dir", defaultStateDir())
	if service {
		dir = "/var/lib/louisiane"
	}
	if s, _ := relay.LoadSettings(dir); len(s.Names) > 0 && !a.has("force") {
		return fmt.Errorf("a relay is already set up in %s; change its password with `lal relay passwd`, or pass --force to set it up again", dir)
	}
	names := a.all("name")
	if len(names) == 0 {
		names = detectNames()
	}
	set := relay.Settings{Listen: a.str("listen", ":8788"), Names: names, DashUser: "owner"}
	_, port, err := splitPort(set.Listen)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", set.Listen, err)
	}

	if service {
		if err := ensureUser("louisiane", dir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := set.Save(dir); err != nil {
		return err
	}
	_, fp, err := pin.LoadOrCreate(dir, names)
	if err != nil {
		return err
	}
	fmt.Printf("louisiane relay set up in %s, reached as %s.\n", dir, strings.Join(names, ", "))
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Println("\nThe dashboard password is your authority on the mesh: messages you send with it")
		fmt.Println("reach every agent as their owner's. Choose one only you know.")
		if err := setPassword(dir); err != nil {
			return err
		}
	} else {
		fmt.Println("No terminal, so no dashboard password yet: the dashboard stays locked until you run `lal relay passwd`.")
	}

	if service {
		if err := chownTree(dir, "louisiane"); err != nil {
			return err
		}
		if err := installSelf("/usr/local/bin/lal"); err != nil {
			return err
		}
		if err := os.WriteFile("/etc/systemd/system/louisiane-relay.service", []byte(relayUnit), 0o644); err != nil {
			return err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "louisiane-relay"}, {"restart", "louisiane-relay"}} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
			}
		}
		fmt.Println("Running as the louisiane-relay service, as user louisiane.")
	} else {
		fmt.Printf("\nStart it with:\n\n    lal relay --state-dir %s\n\n(as root with systemd, `lal relay init` installs it as a service instead)\n", dir)
	}
	fmt.Printf("\nDashboard: https://%s/   (user owner; the browser warns once about the\nself-signed certificate, whose fingerprint is %s)\n",
		net.JoinHostPort(names[0], strconv.Itoa(port)), fp)
	invite := "lal invite HOST"
	if service {
		invite = "sudo lal invite HOST"
	}
	fmt.Printf("\nNext, for each machine with agents:\n\n    %s\n\nand run the line it prints on that machine.\n", invite)
	return nil
}

func haveSystemd() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

func ensureUser(name, home string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	out, err := exec.Command("useradd", "--system", "--home-dir", home, "--no-create-home", "--shell", "/usr/sbin/nologin", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd %s: %v: %s", name, err, out)
	}
	return nil
}

func chownTree(dir, name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// installSelf copies this binary to path unless it is already there.
func installSelf(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil || exe == path {
		return err
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := path + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readNewPassword asks twice, unechoed.
func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	read := func(prompt string) (string, error) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	pw, err := read("dashboard password: ")
	if err != nil {
		return "", err
	}
	if len(pw) < 8 {
		return "", errors.New("use at least 8 characters: it is the owner's authority on the mesh")
	}
	again, err := read("again: ")
	if err != nil {
		return "", err
	}
	if pw != again {
		return "", errors.New("the passwords differ")
	}
	return pw, nil
}

func setPassword(dir string) error {
	pw, err := readNewPassword()
	if err != nil {
		return err
	}
	h, err := dash.HashPassword(pw)
	if err != nil {
		return err
	}
	return relay.WriteDashHash(dir, h)
}

func relayPasswd(argv []string) error {
	a, err := parse(argv, "state-dir")
	if err != nil {
		return err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("lal relay passwd asks for the password at a terminal")
	}
	dir := a.str("state-dir", defaultStateDir())
	if err := setPassword(dir); err != nil {
		return err
	}
	fmt.Println("Dashboard password set; the relay uses it at once.")
	return nil
}

// An invite token carries everything `lal join` needs on one line: the
// relay's addresses, its certificate's fingerprint and the one-time code.
const tokenPrefix = "lal1."

type invite struct {
	URLs []string `json:"u"`
	Pin  string   `json:"p"`
	Code string   `json:"c"`
}

func encodeInvite(i invite) string {
	b, _ := json.Marshal(i)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeInvite(s string) (invite, error) {
	var i invite
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), tokenPrefix))
	if err == nil {
		err = json.Unmarshal(b, &i)
	}
	if err != nil || len(i.URLs) == 0 || i.Pin == "" || i.Code == "" {
		return i, errors.New("that is not a whole invite token; copy the full lal1.… line `lal invite` printed")
	}
	return i, nil
}

func cmdInvite(argv []string) error {
	a, err := parse(argv, "state-dir")
	if err != nil {
		return err
	}
	if len(a.pos) != 1 {
		return errors.New("usage: lal invite HOST")
	}
	var resp proto.InviteResp
	if err := admin(a).Do(context.Background(), "POST", "/admin/invite", proto.InviteReq{Host: a.pos[0]}, &resp); err != nil {
		return fmt.Errorf("%w (run this on the relay's machine, as the user the relay runs as)", err)
	}
	fmt.Printf("Invite for %s, valid for 10 minutes. On %s, run:\n\n    lal join %s\n\n", resp.Host, resp.Host,
		encodeInvite(invite{URLs: resp.URLs, Pin: resp.Pin, Code: resp.Code}))
	return nil
}

func cmdJoin(argv []string) error {
	a, err := parse(argv, "relay", "pin")
	if err != nil {
		return err
	}
	var inv invite
	switch {
	case len(a.pos) == 1 && strings.HasPrefix(a.pos[0], tokenPrefix):
		if inv, err = decodeInvite(a.pos[0]); err != nil {
			return err
		}
	case len(a.pos) == 1 && a.str("relay", "") != "" && a.str("pin", "") != "":
		inv = invite{URLs: []string{a.str("relay", "")}, Pin: a.str("pin", ""), Code: a.pos[0]}
	default:
		return errors.New("usage: lal join TOKEN (the line `lal invite` printed)")
	}
	if old, err := client.LoadConfig(); err == nil && !a.has("force") {
		return fmt.Errorf("this machine already joined %s as %s; pass --force to join again", old.Relay, old.Host)
	}
	var resp proto.JoinResp
	var tried []string
	joined := ""
	for _, u := range inv.URLs {
		err := client.NewPinned(u, inv.Pin).Do(context.Background(), "POST", "/v1/join", proto.JoinReq{Code: inv.Code}, &resp)
		var ae *client.APIError
		if errors.As(err, &ae) {
			return err // the relay answered: the code is bad or used
		}
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%v)", u, err))
			continue
		}
		joined = u
		break
	}
	if joined == "" {
		return fmt.Errorf("could not reach the relay at any of its addresses:\n  %s", strings.Join(tried, "\n  "))
	}
	cfg := &client.Config{Relay: joined, Pin: inv.Pin, Host: resp.Host, Token: resp.Token}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("Joined the relay at %s as %s.\n", joined, resp.Host)
	return offerConnectors(a)
}

var harnesses = []string{"claude", "pi", "omp"}

// offerConnectors installs connectors for the agents found on PATH: asking
// at a terminal, without asking with --yes.
func offerConnectors(a *args) error {
	if a.has("no-install") {
		return nil
	}
	var found []string
	for _, k := range harnesses {
		if _, err := exec.LookPath(k); err == nil {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		fmt.Println("No claude, pi or omp on PATH; install a connector later with `lal install claude|pi|omp`.")
		return nil
	}
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	in := bufio.NewReader(os.Stdin)
	for _, k := range found {
		yes := a.has("yes")
		if !yes && tty {
			fmt.Printf("Install the louisiane connector for %s, so its sessions can join the mesh? [Y/n] ", k)
			line, _ := in.ReadString('\n')
			ans := strings.ToLower(strings.TrimSpace(line))
			yes = ans == "" || ans == "y" || ans == "yes"
		}
		if !yes {
			fmt.Printf("  later: lal install %s\n", k)
			continue
		}
		if err := installConnector(k); err != nil {
			return err
		}
	}
	fmt.Println("\nThen, in each project that should be on the mesh: lal init")
	return nil
}

func cmdInstall(argv []string) error {
	a, err := parse(argv)
	if err != nil {
		return err
	}
	if len(a.pos) == 0 {
		return offerConnectors(a)
	}
	for _, k := range a.pos {
		if err := installConnector(k); err != nil {
			return err
		}
	}
	return nil
}

func installConnector(kind string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	switch kind {
	case "claude":
		if _, err := exec.LookPath("claude"); err != nil {
			return errors.New("claude is not on PATH")
		}
		exec.Command("claude", "mcp", "remove", "--scope", "user", "louisiane").Run()
		out, err := exec.Command("claude", "mcp", "add", "--scope", "user", "louisiane", "--", exe, "mcp").CombinedOutput()
		if err != nil {
			return fmt.Errorf("claude mcp add: %v: %s", err, out)
		}
		fmt.Println(`Installed for claude: Claude Code sessions started from now on run the connector.
  If Claude asks for permission each time it runs lal, allow "Bash(lal:*)" in /permissions.`)
	case "pi", "omp":
		dir := filepath.Join(home, ".pi", "agent", "extensions")
		if kind == "pi" && os.Getenv("PI_CODING_AGENT_DIR") != "" {
			dir = filepath.Join(os.Getenv("PI_CODING_AGENT_DIR"), "extensions")
		}
		if kind == "omp" {
			dir = filepath.Join(home, ".omp", "agent", "extensions")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		src := strings.NewReplacer("__LAL__", exe, "__KIND__", kind).Replace(extensionTS)
		path := filepath.Join(dir, "louisiane.ts")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			return err
		}
		fmt.Printf("Installed for %s: %s; sessions started from now on run the connector.\n", kind, path)
	default:
		return fmt.Errorf("no connector for %q yet (claude, pi and omp have one)", kind)
	}
	return nil
}

// cmdInit puts a project on the mesh: the louisiane block in its AGENTS.md,
// and @AGENTS.md in its CLAUDE.md for Claude Code.
func cmdInit(argv []string) error {
	a, err := parse(argv, "as")
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := client.DetectProject(cwd, "")
	if err != nil {
		return err
	}
	prime := "`lal prime`"
	if as := a.str("as", ""); as != "" {
		prime = "`lal prime --as " + as + "`"
	}
	block := "## louisiane\nThis project is on the louisiane agent mesh. At the start of every session, run\n" + prime + " and follow what it prints.\n"
	changed, err := ensureIn(filepath.Join(p.Root, "AGENTS.md"), "lal prime", block)
	if err != nil {
		return err
	}
	report(p.Root, "AGENTS.md", changed)
	changed, err = ensureIn(filepath.Join(p.Root, "CLAUDE.md"), "@AGENTS.md", "@AGENTS.md\n")
	if err != nil {
		return err
	}
	report(p.Root, "CLAUDE.md", changed)
	fmt.Printf("Agents that start in %s now join the mesh as %s (once their connector is installed).\n", p.Root, p.Name)
	return nil
}

// ensureIn appends text to path unless path already contains marker.
func ensureIn(path, marker, text string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	s := string(b)
	if strings.Contains(s, marker) {
		return false, nil
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" {
		s += "\n"
	}
	return true, os.WriteFile(path, []byte(s+text), 0o644)
}

func report(root, name string, changed bool) {
	if changed {
		fmt.Printf("Updated %s\n", filepath.Join(root, name))
	} else {
		fmt.Printf("%s already has it\n", filepath.Join(root, name))
	}
}
