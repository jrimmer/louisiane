package relay

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/louisiane/internal/dash"
	"github.com/jrimmer/louisiane/internal/proto"
)

// Owner is the sender name of messages from the mesh's owner. Agents give
// it their owner's authority, so only the dashboard's login can send as it:
// a password the owner knows and the agents don't. Not the admin socket,
// whose file permissions don't tell the owner from agents running as the
// same user.
const Owner = "owner"

// ownerOK reports whether req carries the dashboard's login (only when
// that is on).
func (r *Relay) ownerOK(req *http.Request) bool {
	if !r.cfg.DashAuth {
		return false
	}
	u, p, ok := req.BasicAuth()
	return ok && u == r.cfg.DashUser && dash.CheckPassword(r.dashHash(), p)
}

func (r *Relay) handleOwnerSend(w http.ResponseWriter, req *http.Request) error {
	if !r.ownerOK(req) {
		return errf(403, "only the owner can send as owner: the dashboard's login is needed (in the browser, or `lal say --owner`, which asks for it), and the relay must run with it on")
	}
	var body proto.SendReq
	if err := readJSON(req, &body); err != nil {
		return err
	}
	resp, err := r.Send(Owner, body)
	if err != nil {
		return err
	}
	writeJSON(w, 200, resp)
	return nil
}

// LogQuery selects events from the log for catching up.
type LogQuery struct {
	Since time.Time
	Room  string // without '#'
	Who   string // a project or member: messages from or to it
	Limit int
	All   bool // every event type, not just conversation
}

func parseLogQuery(req *http.Request) LogQuery {
	q := req.URL.Query()
	lq := LogQuery{Since: time.Now().Add(-2 * time.Hour), Room: strings.TrimPrefix(q.Get("room"), "#"), Who: q.Get("who"), Limit: 500}
	if d, err := time.ParseDuration(q.Get("since")); err == nil {
		lq.Since = time.Now().Add(-d)
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		lq.Limit = min(n, 5000)
	}
	lq.All = q.Get("all") != ""
	return lq
}

// readLog reads matching events from events.jsonl and its rotations,
// oldest first, keeping the newest Limit.
func (r *Relay) readLog(q LogQuery) ([]proto.Event, error) {
	path := r.events.path
	var out []proto.Event
	since := q.Since.UnixMilli()
	for _, p := range []string{path + ".3", path + ".2", path + ".1", path} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var ev proto.Event
			if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.At < since || !q.match(ev) {
				continue
			}
			out = append(out, ev)
			if len(out) > q.Limit*2 {
				out = append(out[:0:0], out[len(out)-q.Limit:]...)
			}
		}
		f.Close()
	}
	if len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}

func (q LogQuery) match(ev proto.Event) bool {
	switch ev.Type {
	case proto.EventMsg, proto.EventLeader, proto.EventStatus:
	case proto.EventJoined, proto.EventLeft, proto.EventRefused, proto.EventRoom:
		if !q.All {
			return false
		}
	default:
		return false
	}
	if q.Room != "" && ev.Room != q.Room && !(ev.Type != proto.EventMsg && ev.Project == q.Room) {
		return false
	}
	if q.Who != "" {
		hit := func(s string) bool { return s == q.Who || strings.HasPrefix(s, q.Who+"/") }
		if !hit(ev.From) && !hit(ev.To) && !hit(ev.Member) && ev.Room != q.Who && ev.Project != q.Who {
			return false
		}
	}
	return true
}

func (r *Relay) handleLog(w http.ResponseWriter, req *http.Request) error {
	if err := r.reader(req); err != nil && !r.ownerOK(req) {
		return err
	}
	evs, err := r.readLog(parseLogQuery(req))
	if err != nil {
		return err
	}
	writeJSON(w, 200, evs)
	return nil
}

func (r *Relay) handleFeed(w http.ResponseWriter, req *http.Request) error {
	q := parseLogQuery(req)
	evs, err := r.readLog(q)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return dash.Feed(w, dash.FeedData{Events: evs, Since: req.URL.Query().Get("since"), Room: q.Room, Who: q.Who,
		Who2: r.Who(), CanSend: r.cfg.DashAuth})
}
