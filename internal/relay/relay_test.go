package relay

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jrimmer/louisiane/internal/proto"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newRelay(t *testing.T) (*Relay, *clock) {
	t.Helper()
	r, err := Open(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	r.SetClock(c.now)
	return r, c
}

// join connects a member and returns its name and stream.
func join(t *testing.T, r *Relay, c *clock, host, key, name, kind string) (string, *sub) {
	t.Helper()
	s := newSub()
	r.mu.Lock()
	m, _, err := r.connect(host, ConnectReq{Key: key, Name: name, Kind: kind}, s)
	if err == nil {
		r.subscribe(s, "")
	}
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Second) // keep seniority distinct
	return m.Name, s
}

// drain returns the events waiting on s.
func drain(s *sub) []proto.Event {
	var out []proto.Event
	for {
		select {
		case e := <-s.ch:
			out = append(out, e.ev)
		default:
			return out
		}
	}
}

func msgs(evs []proto.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == proto.EventMsg {
			out = append(out, e.Body)
		}
	}
	return out
}

func leave(r *Relay, c *clock, name string) {
	r.mu.Lock()
	m := r.members[name]
	for s := range m.subs {
		r.unsubscribe(s)
		r.detach(m, s)
	}
	r.mu.Unlock()
	c.t = c.t.Add(r.cfg.Grace + time.Second)
	r.Sweep()
}

func code(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code
	}
	return 0
}

func TestLeaderElectionHandoffAndFailover(t *testing.T) {
	r, c := newRelay(t)
	a, _ := join(t, r, c, "desk", "k/bez", "bez", "claude")
	b, sb := join(t, r, c, "desk", "k/bez", "bez", "omp")
	d, _ := join(t, r, c, "desk", "k/bez", "bez", "pi")
	if p := r.projects["bez"]; p.Leader != a || p.Term != 1 {
		t.Fatalf("first member should lead at term 1, got %s term %d", p.Leader, p.Term)
	}
	if _, err := r.Lead(b, proto.LeadReq{Action: "handoff", To: d}); code(err) != 403 {
		t.Fatalf("a non-leader handed off: %v", err)
	}
	if _, err := r.Lead(a, proto.LeadReq{Action: "handoff", To: b}); err != nil {
		t.Fatal(err)
	}
	if p := r.projects["bez"]; p.Leader != b || p.Term != 2 {
		t.Fatalf("handoff: got %s term %d", p.Leader, p.Term)
	}
	if !slices.ContainsFunc(drain(sb), func(e proto.Event) bool { return e.Type == proto.EventLeader && e.Member == b }) {
		t.Fatal("new leader was not told")
	}
	leave(r, c, b)
	// The most senior remaining member takes over.
	if p := r.projects["bez"]; p.Leader != a || p.Term != 3 {
		t.Fatalf("failover: got %s term %d, want %s term 3", p.Leader, p.Term, a)
	}
	leave(r, c, a)
	leave(r, c, d)
	if r.projects["bez"] != nil {
		t.Fatal("an empty project lingered")
	}
}

func TestAddressing(t *testing.T) {
	r, c := newRelay(t)
	a, sa := join(t, r, c, "desk", "k/bez", "bez", "claude")
	b, sb := join(t, r, c, "desk", "k/bez", "bez", "omp")
	s, ss := join(t, r, c, "rack", "k/api", "api", "pi")

	resp, err := r.Send(s, proto.SendReq{To: "#all", Body: "going down"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Delivered, []string{a, b}) {
		t.Fatalf("#all reached %v", resp.Delivered)
	}
	if got := msgs(drain(ss)); len(got) != 0 {
		t.Fatalf("sender heard itself: %v", got)
	}
	drain(sa)
	drain(sb)

	if resp, err = r.Send(s, proto.SendReq{To: "bez", Body: "to the project"}); err != nil || !slices.Equal(resp.Delivered, []string{a}) {
		t.Fatalf("project DM: %v %v", resp, err)
	}
	if resp, err = r.Send(s, proto.SendReq{To: b, Body: "direct"}); err != nil || !slices.Equal(resp.Delivered, []string{b}) {
		t.Fatalf("member DM: %v %v", resp, err)
	}
	if _, err = r.Send(a, proto.SendReq{To: "bez", Body: "me?"}); code(err) != 400 {
		t.Fatalf("leader DMing its own project: %v", err)
	}
	if resp, err = r.Send(b, proto.SendReq{To: "#bez", Body: "team"}); err != nil || !slices.Equal(resp.Delivered, []string{a}) {
		t.Fatalf("project room: %v %v", resp, err)
	}
	if _, err = r.Send(s, proto.SendReq{To: "#nobody", Body: "hello?"}); code(err) != 404 {
		t.Fatalf("empty room: %v", err)
	}
	if _, err = r.Send(s, proto.SendReq{To: "ghost", Body: "hello?"}); code(err) != 404 {
		t.Fatalf("unknown project: %v", err)
	}
	if n := len(r.refusals); n != 3 {
		t.Fatalf("refusals recorded: %d, want 3", n)
	}
	if got := msgs(drain(sa)); !slices.Equal(got, []string{"to the project", "team"}) {
		t.Fatalf("leader got %v", got)
	}

	// Ad-hoc rooms.
	if _, err := r.Room(b, "#sb-maint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Room(b, "#bez", true); code(err) != 400 {
		t.Fatalf("joined a project's room by hand: %v", err)
	}
	if resp, err = r.Send(s, proto.SendReq{To: "#sb-maint", Body: "prep"}); err != nil || !slices.Equal(resp.Delivered, []string{b}) {
		t.Fatalf("topic room: %v %v", resp, err)
	}
}

func TestRepliesAndThreads(t *testing.T) {
	r, c := newRelay(t)
	a, sa := join(t, r, c, "desk", "k/bez", "bez", "claude")
	b, sb := join(t, r, c, "desk", "k/bez", "bez", "omp")
	s, ss := join(t, r, c, "rack", "k/api", "api", "pi")

	q, err := r.Send(s, proto.SendReq{To: "#all", Body: "port 4000?", Ask: true})
	if err != nil {
		t.Fatal(err)
	}
	drain(sa)
	drain(sb)
	// A reply goes to the author only...
	rep, err := r.Send(a, proto.SendReq{ReplyTo: q.ID, Body: "mine"})
	if err != nil || !slices.Equal(rep.Delivered, []string{s}) {
		t.Fatalf("reply: %v %v", rep, err)
	}
	// ...or to the room, and the author, with --room.
	rep2, err := r.Send(b, proto.SendReq{ReplyTo: q.ID, Body: "also", InRoom: true})
	if want := []string{a, s}; err != nil || !slices.Equal(rep2.Delivered, slices.Sorted(slices.Values(want))) {
		t.Fatalf("room reply: %v %v", rep2, err)
	}
	evs := drain(ss)
	if len(evs) != 2 || evs[0].Thread != q.ID || evs[0].Depth != 1 || evs[1].ReplyTo != q.ID {
		t.Fatalf("threading: %+v", evs)
	}

	// A watch collects replies instead of the connector.
	r.mu.Lock()
	r.members[s].watching[q.ID]++
	r.mu.Unlock()
	if _, err := r.Send(a, proto.SendReq{ReplyTo: q.ID, Body: "again"}); err != nil {
		t.Fatal(err)
	}
	if got := msgs(drain(ss)); len(got) != 0 {
		t.Fatalf("watched reply reached the connector: %v", got)
	}
	_ = b
}

func TestDepthAndRateLimits(t *testing.T) {
	r, c := newRelay(t)
	r.cfg.MaxDepth = 3
	a, _ := join(t, r, c, "desk", "k/a", "a", "claude")
	b, _ := join(t, r, c, "desk", "k/b", "b", "omp")
	resp, err := r.Send(a, proto.SendReq{To: b, Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	from := []string{b, a}
	for i := 0; i < 3; i++ {
		if resp, err = r.Send(from[i%2], proto.SendReq{ReplyTo: resp.ID, Body: "and you"}); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if _, err = r.Send(from[1], proto.SendReq{ReplyTo: resp.ID, Body: "and you"}); code(err) != 429 {
		t.Fatalf("thread depth not limited: %v", err)
	}
	for i := 0; i < r.cfg.AllPerMinute; i++ {
		if _, err := r.Send(b, proto.SendReq{To: "#all", Body: "news"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Send(b, proto.SendReq{To: "#all", Body: "more news"}); code(err) != 429 {
		t.Fatalf("#all not limited: %v", err)
	}
}

func TestResumeFromRing(t *testing.T) {
	r, c := newRelay(t)
	a, sa := join(t, r, c, "desk", "k/a", "a", "claude")
	b, _ := join(t, r, c, "desk", "k/b", "b", "omp")
	if _, err := r.Send(b, proto.SendReq{To: a, Body: "one"}); err != nil {
		t.Fatal(err)
	}
	evs := drain(sa)
	last := EventID(evs[len(evs)-1])
	// The stream drops; a message arrives inside the grace.
	r.mu.Lock()
	m := r.members[a]
	r.unsubscribe(sa)
	r.detach(m, sa)
	r.mu.Unlock()
	if _, err := r.Send(b, proto.SendReq{To: a, Body: "two"}); err != nil {
		t.Fatalf("send during the grace: %v", err)
	}
	s := newSub()
	r.mu.Lock()
	if _, _, err := r.connect("desk", ConnectReq{Key: "k/a", Name: "a", Member: a}, s); err != nil {
		t.Fatal(err)
	}
	missed, gap := r.subscribe(s, last)
	_, _, gap2 := func() ([]*entry, bool, bool) { m, g := r.subscribe(newSub(), "zzzz-1"); return m, g, g }()
	r.mu.Unlock()
	if gap || len(missed) != 1 || missed[0].ev.Body != "two" {
		t.Fatalf("resume: gap %v, missed %d", gap, len(missed))
	}
	if !gap2 {
		t.Fatal("a position from another epoch should be a gap")
	}
}

func TestProjectNames(t *testing.T) {
	r, c := newRelay(t)
	join(t, r, c, "desk", "github.com/x/api", "api", "claude")
	n, _ := join(t, r, c, "rack", "rack:/srv/api", "api", "pi")
	if n[:len("api@rack/")] != "api@rack/" {
		t.Fatalf("clash got %s", n)
	}
	m, _ := join(t, r, c, "laptop", "github.com/x/api", "whatever", "omp")
	if m[:4] != "api/" {
		t.Fatalf("same remote should be the same project, got %s", m)
	}
	if _, err := r.Send(m, proto.SendReq{To: "owner", Body: "for the owner"}); err != nil {
		t.Fatalf("message to the owner: %v", err)
	}
	if _, err := r.Send(Owner, proto.SendReq{To: "#all", Body: "freeze"}); err != nil {
		t.Fatalf("owner broadcast: %v", err)
	}
}
