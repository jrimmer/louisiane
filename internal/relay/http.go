package relay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/louisiane/internal/dash"
	"github.com/jrimmer/louisiane/internal/pin"
	"github.com/jrimmer/louisiane/internal/proto"
)

const (
	heartbeat    = 15 * time.Second
	writeTimeout = 30 * time.Second
	MemberHeader = "X-Lal-Member"
	AsHostHeader = "X-Lal-As-Host"
)

type ctxKey int

const adminKey ctxKey = 0

func isAdmin(r *http.Request) bool { return r.Context().Value(adminKey) != nil }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeJSON(w, ae.code, proto.ErrorResp{Error: ae.msg})
		return
	}
	writeJSON(w, http.StatusInternalServerError, proto.ErrorResp{Error: err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return errf(400, "bad JSON: %v", err)
	}
	return nil
}

// host authenticates the caller's machine.
func (r *Relay) host(req *http.Request) (string, error) {
	tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", errf(401, "missing host token; run `lal join` on this host")
	}
	h, ok := r.hosts.lookup(tok)
	if !ok {
		return "", errf(401, "unknown host token; this host may have been removed, so join it again")
	}
	r.mu.Lock()
	r.hostSeen[h] = r.ms()
	r.mu.Unlock()
	return h, nil
}

// caller is the member (or "@host") a call speaks for.
func (r *Relay) caller(req *http.Request) (string, error) {
	h, err := r.host(req)
	if err != nil {
		return "", err
	}
	if req.Header.Get(AsHostHeader) != "" {
		return "@" + h, nil
	}
	name := req.Header.Get(MemberHeader)
	if name == "" {
		return "", errf(409, "this session is not connected to the mesh; run `lal prime`")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.members[name]
	if m == nil {
		return "", notConnected(name)
	}
	if m.Host != h {
		return "", errf(403, "%s belongs to %s, not %s", name, m.Host, h)
	}
	m.LastSeen = r.ms()
	return name, nil
}

// sameOrigin refuses state-changing requests a browser sends on behalf of
// another site. The dashboard's login is cached by the browser, so without
// this any page the owner visits could post as the owner. Browsers mark
// every cross-site POST with Origin and Sec-Fetch-Site; the CLI sends
// neither, so it is not affected. Browser requests must also be JSON, which
// a cross-site page cannot send without a preflight the relay never grants.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet || req.Method == http.MethodHead {
			next.ServeHTTP(w, req)
			return
		}
		origin, site := req.Header.Get("Origin"), req.Header.Get("Sec-Fetch-Site")
		if origin != "" || site != "" {
			ok := site == "" || site == "same-origin" || site == "none"
			if u, err := url.Parse(origin); origin != "" && (err != nil || u.Host != req.Host) {
				ok = false
			}
			ct := req.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				ok = false
			}
			if !ok {
				writeJSON(w, http.StatusForbidden, proto.ErrorResp{Error: "cross-site request refused"})
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

// Handler serves the API, the streams and the dashboard. admin is set for
// the local admin socket.
func (r *Relay) Handler(board *dash.Board) http.Handler {
	return sameOrigin(r.mux(board))
}

func (r *Relay) mux(board *dash.Board) http.Handler {
	mux := http.NewServeMux()
	api := func(pattern string, h func(w http.ResponseWriter, req *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
			if err := h(w, req); err != nil {
				writeErr(w, err)
			}
		})
	}
	api("POST /v1/join", r.handleJoin)
	api("GET /v1/stream", r.handleStream)
	api("POST /v1/send", r.handleSend)
	api("GET /v1/watch", r.handleWatch)
	api("GET /v1/who", r.handleWho)
	api("POST /v1/rooms/{room}/{action}", r.handleRoom)
	api("POST /v1/lead", r.handleLead)
	api("POST /v1/status", r.handleStatus)
	api("POST /v1/bye", r.handleBye)
	api("GET /v1/tail", r.handleTail)
	api("GET /v1/snapshot", r.handleSnapshot)
	api("GET /v1/log", r.handleLog)
	api("GET /v1/chat", r.handleChat)
	api("POST /owner/send", r.handleOwnerSend)
	api("POST /admin/invite", r.adminOnly(r.handleInvite))
	api("GET /admin/hosts", r.adminOnly(r.handleHosts))
	api("DELETE /admin/hosts/{name}", r.adminOnly(r.handleRemoveHost))
	if board != nil {
		mux.Handle("GET /feed", r.dashAuth(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if err := r.handleFeed(w, req); err != nil {
				writeErr(w, err)
			}
		})))
		mux.Handle("/", r.dashAuth(board.Handler()))
	}
	return mux
}

func (r *Relay) adminOnly(h func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, req *http.Request) error {
		if !isAdmin(req) {
			return errf(403, "admin calls are served only on the relay host's admin socket")
		}
		return h(w, req)
	}
}

// reader allows the admin socket or any joined host.
func (r *Relay) reader(req *http.Request) error {
	if isAdmin(req) {
		return nil
	}
	_, err := r.host(req)
	return err
}

func (r *Relay) handleJoin(w http.ResponseWriter, req *http.Request) error {
	var body proto.JoinReq
	if err := readJSON(req, &body); err != nil {
		return err
	}
	host, token, err := r.redeem(body.Code)
	if err != nil {
		return err
	}
	r.log.Printf("host %s joined", host)
	writeJSON(w, 200, proto.JoinResp{Host: host, Token: token})
	return nil
}

func (r *Relay) handleInvite(w http.ResponseWriter, req *http.Request) error {
	var body proto.InviteReq
	if err := readJSON(req, &body); err != nil {
		return err
	}
	host := cleanName(body.Host)
	if host == "" {
		return errf(400, "invite needs a host name")
	}
	code, exp := r.newInvite(host)
	writeJSON(w, 200, proto.InviteResp{Code: code, Host: host, ExpiresAt: exp, Pin: r.Pin, URLs: r.urls()})
	return nil
}

func (r *Relay) urls() []string {
	var out []string
	for _, n := range r.cfg.Names {
		out = append(out, "https://"+net.JoinHostPort(n, strconv.Itoa(r.cfg.Port)))
	}
	return out
}

func (r *Relay) handleHosts(w http.ResponseWriter, req *http.Request) error {
	r.mu.Lock()
	counts := map[string]int{}
	for _, m := range r.members {
		counts[m.Host]++
	}
	seen := r.hostSeen
	var out []proto.HostInfo
	for _, h := range r.hosts.list() {
		out = append(out, proto.HostInfo{Name: h.Name, CreatedAt: h.CreatedAt, LastSeen: seen[h.Name], Members: counts[h.Name]})
	}
	r.mu.Unlock()
	writeJSON(w, 200, out)
	return nil
}

func (r *Relay) handleRemoveHost(w http.ResponseWriter, req *http.Request) error {
	name := req.PathValue("name")
	ok, err := r.hosts.remove(name)
	if err != nil {
		return err
	}
	if !ok {
		return errf(404, "no host %s", name)
	}
	r.mu.Lock()
	for _, m := range r.members {
		if m.Host == name {
			r.remove(m, "host removed")
		}
	}
	r.mu.Unlock()
	writeJSON(w, 200, map[string]string{"removed": name})
	return nil
}

func (r *Relay) handleSend(w http.ResponseWriter, req *http.Request) error {
	from, err := r.caller(req)
	if err != nil {
		return err
	}
	var body proto.SendReq
	if err := readJSON(req, &body); err != nil {
		return err
	}
	resp, err := r.Send(from, body)
	if err != nil {
		return err
	}
	writeJSON(w, 200, resp)
	return nil
}

func (r *Relay) handleWho(w http.ResponseWriter, req *http.Request) error {
	if err := r.reader(req); err != nil {
		return err
	}
	writeJSON(w, 200, r.Who())
	return nil
}

func (r *Relay) handleRoom(w http.ResponseWriter, req *http.Request) error {
	name, err := r.caller(req)
	if err != nil {
		return err
	}
	action := req.PathValue("action")
	if action != "join" && action != "leave" {
		return errf(404, "room action must be join or leave")
	}
	ms, err := r.Room(name, req.PathValue("room"), action == "join")
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"room": strings.TrimPrefix(req.PathValue("room"), "#"), "members": ms})
	return nil
}

func (r *Relay) handleLead(w http.ResponseWriter, req *http.Request) error {
	name, err := r.caller(req)
	if err != nil {
		return err
	}
	var body proto.LeadReq
	if err := readJSON(req, &body); err != nil {
		return err
	}
	p, err := r.Lead(name, body)
	if err != nil {
		return err
	}
	writeJSON(w, 200, p)
	return nil
}

func (r *Relay) handleStatus(w http.ResponseWriter, req *http.Request) error {
	name, err := r.caller(req)
	if err != nil {
		return err
	}
	var body proto.StatusReq
	if err := readJSON(req, &body); err != nil {
		return err
	}
	if err := r.Status(name, body.WorkingOn); err != nil {
		return err
	}
	writeJSON(w, 200, body)
	return nil
}

func (r *Relay) handleBye(w http.ResponseWriter, req *http.Request) error {
	name, err := r.caller(req)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if m := r.members[name]; m != nil {
		r.remove(m, "left")
	}
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (r *Relay) handleSnapshot(w http.ResponseWriter, req *http.Request) error {
	if err := r.reader(req); err != nil {
		return err
	}
	writeJSON(w, 200, r.Snapshot())
	return nil
}

// sse writes events to one stream.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	return &sse{w: w, rc: http.NewResponseController(w)}
}

func (s *sse) event(ev proto.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	id := ""
	if ev.Seq > 0 {
		id = "id: " + EventID(ev) + "\n"
	}
	_, err = fmt.Fprintf(s.w, "%sdata: %s\n\n", id, b)
	return err
}

func (s *sse) ping() error {
	s.rc.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := io.WriteString(s.w, ": ping\n\n")
	return err
}

func (s *sse) flush() error { return s.rc.Flush() }

// pump writes what s receives until the request ends, the stream is
// closed, or until (when non-zero).
func (r *Relay) pump(ctx context.Context, out *sse, s *sub, until time.Time) error {
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	var deadline <-chan time.Time
	if !until.IsZero() {
		d := time.NewTimer(time.Until(until))
		defer d.Stop()
		deadline = d.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		case <-deadline:
			return nil
		case <-t.C:
			if err := out.ping(); err != nil {
				return err
			}
		case e := <-s.ch:
			r.mu.Lock()
			lost := s.lost
			s.lost = false
			r.mu.Unlock()
			if lost {
				if err := out.event(proto.Event{Type: proto.EventGap, Body: "this stream fell behind and lost events"}); err != nil {
					return err
				}
			}
			if err := out.event(e.ev); err != nil {
				return err
			}
		}
		if err := out.flush(); err != nil {
			return err
		}
	}
}

func (r *Relay) handleStream(w http.ResponseWriter, req *http.Request) error {
	host, err := r.host(req)
	if err != nil {
		return err
	}
	q := req.URL.Query()
	pid, _ := strconv.Atoi(q.Get("pid"))
	creq := ConnectReq{Key: q.Get("key"), Name: q.Get("name"), Root: q.Get("root"), Kind: q.Get("kind"), PID: pid,
		Member: q.Get("member")}
	if rooms := q.Get("rooms"); rooms != "" {
		creq.Rooms = strings.Split(rooms, ",")
	}
	s := newSub()
	r.mu.Lock()
	m, hello, err := r.connect(host, creq, s)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	missed, gap := r.subscribe(s, req.Header.Get("Last-Event-ID"))
	first := proto.Event{Type: proto.EventHello, Seq: r.seq, Epoch: r.epoch, At: r.ms(), Member: m.Name, Hello: hello}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.unsubscribe(s)
		if r.members[m.Name] == m {
			r.detach(m, s)
		}
		r.mu.Unlock()
	}()
	out := startSSE(w)
	if err := out.event(first); err != nil {
		return nil
	}
	if gap {
		if err := out.event(proto.Event{Type: proto.EventGap, Body: "the relay restarted or the stream was away too long; messages sent meanwhile were missed"}); err != nil {
			return nil
		}
	}
	for _, e := range missed {
		if err := out.event(e.ev); err != nil {
			return nil
		}
	}
	if err := out.flush(); err != nil {
		return nil
	}
	r.pump(req.Context(), out, s, time.Time{})
	return nil
}

func (r *Relay) handleWatch(w http.ResponseWriter, req *http.Request) error {
	name, err := r.caller(req)
	if err != nil {
		return err
	}
	id := req.URL.Query().Get("reply_to")
	if id == "" {
		return errf(400, "watch needs reply_to")
	}
	wait, _ := strconv.Atoi(req.URL.Query().Get("wait"))
	if wait <= 0 || wait > 3600 {
		wait = 120
	}
	s := newSub()
	s.member, s.replyTo = name, id
	r.mu.Lock()
	m := r.members[name]
	if m == nil {
		r.mu.Unlock()
		return notConnected(name)
	}
	m.watching[id]++
	var missed []*entry
	for _, e := range r.ring {
		if r.wants(s, e) {
			missed = append(missed, e)
		}
	}
	r.subs[s] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.unsubscribe(s)
		if m.watching[id]--; m.watching[id] <= 0 {
			delete(m.watching, id)
		}
		r.mu.Unlock()
	}()
	out := startSSE(w)
	for _, e := range missed {
		if err := out.event(e.ev); err != nil {
			return nil
		}
	}
	if err := out.flush(); err != nil {
		return nil
	}
	r.pump(req.Context(), out, s, time.Now().Add(time.Duration(wait)*time.Second))
	return nil
}

func (r *Relay) handleTail(w http.ResponseWriter, req *http.Request) error {
	if err := r.reader(req); err != nil {
		return err
	}
	s := newSub()
	s.all = true
	r.mu.Lock()
	missed, gap := r.subscribe(s, req.Header.Get("Last-Event-ID"))
	if n, _ := strconv.Atoi(req.URL.Query().Get("back")); n > 0 && req.Header.Get("Last-Event-ID") == "" {
		start := max(len(r.ring)-n, 0)
		for _, e := range r.ring[start:] {
			missed = append(missed, e)
		}
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.unsubscribe(s)
		r.mu.Unlock()
	}()
	out := startSSE(w)
	if gap {
		out.event(proto.Event{Type: proto.EventGap, Body: "events were missed"})
	}
	for _, e := range missed {
		if err := out.event(e.ev); err != nil {
			return nil
		}
	}
	if err := out.flush(); err != nil {
		return nil
	}
	r.pump(req.Context(), out, s, time.Time{})
	return nil
}

// dashAuth puts basic auth in front of the dashboard when it is on.
func (r *Relay) dashAuth(next http.Handler) http.Handler {
	if !r.cfg.DashAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		u, p, ok := req.BasicAuth()
		if !ok || u != r.cfg.DashUser || !dash.CheckPassword(r.dashHash(), p) {
			w.Header().Set("WWW-Authenticate", `Basic realm="louisiane"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// Serve runs the relay on addr (HTTPS) and on the admin socket in its state
// directory until ctx ends.
func (r *Relay) Serve(ctx context.Context, addr string) error {
	cert, fp, err := pin.LoadOrCreate(r.cfg.StateDir, r.cfg.Names)
	if err != nil {
		return err
	}
	r.Pin = fp
	board := dash.NewBoard(r)
	h := r.Handler(board)

	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		return err
	}
	sock := filepath.Join(r.cfg.StateDir, "admin.sock")
	os.Remove(sock)
	aln, err := net.Listen("unix", sock)
	if err != nil {
		ln.Close()
		return err
	}
	os.Chmod(sock, 0o600)

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	asrv := &http.Server{Handler: h, ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, adminKey, true)
	}}
	done := make(chan struct{})
	go r.Run(done)
	go r.feedBoard(done, board)
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	go func() { errc <- asrv.Serve(aln) }()
	r.log.Printf("listening on %s (pin %s), admin socket %s", addr, fp, sock)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	close(done)
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	asrv.Shutdown(sctx)
	os.Remove(sock)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// feedBoard hands the dashboard a fresh snapshot after changes, at most
// four times a second, and at least once a second for the clocks.
func (r *Relay) feedBoard(done <-chan struct{}, board *dash.Board) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		board.Update(r.Snapshot())
		select {
		case <-done:
			return
		case <-t.C:
		case <-r.kick:
			time.Sleep(250 * time.Millisecond)
		}
	}
}
